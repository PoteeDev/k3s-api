package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"text/template"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

var (
	kubeconfig string
	filename   string
	command    string
)

type PodVariables struct {
	ClientName string
	Flag       string
}

type K8sApi struct {
	Config        *rest.Config
	Clientset     *kubernetes.Clientset
	DynamicClient *dynamic.DynamicClient
}

func GetConfig(task string) string {
	s3Client, err := minio.New(fmt.Sprintf("%s:9000", os.Getenv("MINIO_HOST")), &minio.Options{
		Creds:  credentials.NewStaticV4(os.Getenv("MINIO_ACCESS_KEY"), os.Getenv("MINIO_SECRET_KEY"), ""),
		Secure: false,
	})
	if err != nil {
		log.Fatalln(err)
	}
	objName := fmt.Sprintf("%s.yml", task)
	object, err := s3Client.GetObject(context.Background(), "tasks", objName, minio.GetObjectOptions{})
	if err != nil {
		log.Fatalln(err)
	}
	buf := new(bytes.Buffer)
	buf.ReadFrom(object)
	return buf.String()
}

func InitK8sApi(kubeconfig string) *K8sApi {
	var api K8sApi
	var err error
	api.Config, err = rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	api.Clientset, err = kubernetes.NewForConfig(api.Config)
	if err != nil {
		log.Fatal(err)
	}
	api.DynamicClient, err = dynamic.NewForConfig(api.Config)
	if err != nil {
		log.Fatal(err)
	}
	return &api
}

func TeplateManifest(data string, vars PodVariables) (buf bytes.Buffer) {
	tmpl, err := template.New("task").Parse(data)
	if err != nil {
		log.Println(err)
	}
	tmpl.Execute(&buf, vars)
	return buf
}

func (api *K8sApi) ManifestAction(data bytes.Buffer, action func(*unstructured.Unstructured, dynamic.ResourceInterface) error) error {
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data.Bytes()), 100)
	for {
		var rawObj runtime.RawExtension
		if err := decoder.Decode(&rawObj); err != nil {
			break
		}

		obj, gvk, err := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme).Decode(rawObj.Raw, nil, nil)
		if err != nil {
			log.Fatal(err)
		}
		unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			log.Fatal(err)
		}

		unstructuredObj := &unstructured.Unstructured{Object: unstructuredMap}

		gr, err := restmapper.GetAPIGroupResources(api.Clientset.Discovery())
		if err != nil {
			log.Fatal(err)
		}

		mapper := restmapper.NewDiscoveryRESTMapper(gr)
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			log.Fatal(err)
		}

		var dri dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			if unstructuredObj.GetNamespace() == "" {
				unstructuredObj.SetNamespace("default")
			}
			dri = api.DynamicClient.Resource(mapping.Resource).Namespace(unstructuredObj.GetNamespace())
		} else {
			dri = api.DynamicClient.Resource(mapping.Resource)
		}
		err = action(unstructuredObj, dri)
		if err != nil {
			return err
		}
	}
	return nil
}

func (api *K8sApi) ApplyManifest(manifest string, vars PodVariables) error {
	manifestData := TeplateManifest(manifest, vars)
	return api.ManifestAction(manifestData, func(u *unstructured.Unstructured, dri dynamic.ResourceInterface) error {
		// for each object in manifest
		if _, err := dri.Create(context.Background(), u, metav1.CreateOptions{}); err != nil {
			return err
		}
		return nil
	})
}

func (api *K8sApi) DeleteManifest(manifest string, vars PodVariables) error {
	manifestData := TeplateManifest(manifest, vars)
	return api.ManifestAction(manifestData, func(u *unstructured.Unstructured, dri dynamic.ResourceInterface) error {
		// for each object in manifest
		name := u.Object["metadata"].(map[string]interface{})["name"]
		if err := dri.Delete(context.Background(), name.(string), metav1.DeleteOptions{}); err != nil {
			return err
		}
		return nil
	})
}

func Auth(next http.Handler) http.Handler {
	apiKeyHeader := "X-Service-Key"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(apiKeyHeader) != os.Getenv("SRV_KEY") {
			hostIP, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				log.Println("failed to parse remote address", "error", err)
				hostIP = r.RemoteAddr
			}
			log.Println("key not match", "remoteIP", hostIP)
			fmt.Fprintln(w, http.StatusUnauthorized, "invalid api key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type Handlers struct {
	api *K8sApi
}

type Request struct {
	ClientName string `json:"client"`
	TaskName   string `json:"task"`
	Flag       string `json:"flag"`
}

type Response struct {
	Message string `json:"msg"`
}

func (h *Handlers) Run(w http.ResponseWriter, r *http.Request) {
	var req Request
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		json.NewEncoder(w).Encode(&Response{err.Error()})
		return
	}
	manifest := GetConfig(req.TaskName)

	pvars := PodVariables{
		ClientName: req.ClientName,
		Flag:       req.Flag,
	}
	err = h.api.ApplyManifest(manifest, pvars)
	if err != nil {
		json.NewEncoder(w).Encode(&Response{err.Error()})
		return
	}
	podName := fmt.Sprintf("%s-%s", req.TaskName, req.ClientName)
	destroyFunc := func() {
		err = h.api.DeleteManifest(manifest, pvars)
		if err != nil {
			log.Printf("destroy pod %s for error: %v", podName, err)
		}
	}
	podsTimer.CreatePodTimer(podName, destroyFunc)
	resp := Response{"ok"}
	json.NewEncoder(w).Encode(resp)
}

func (h *Handlers) Stop(w http.ResponseWriter, r *http.Request) {
	var req Request
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		json.NewEncoder(w).Encode(&Response{err.Error()})
		return
	}
	manifest := GetConfig(req.TaskName)
	pvars := PodVariables{
		ClientName: req.ClientName,
		Flag:       req.Flag,
	}
	err = h.api.DeleteManifest(manifest, pvars)
	if err != nil {
		json.NewEncoder(w).Encode(&Response{err.Error()})
		return
	}
	podsTimer.StopPodTimer(fmt.Sprintf("%s-%s", req.TaskName, req.ClientName))
	resp := Response{"ok"}
	json.NewEncoder(w).Encode(&resp)
}

type StatusRespone struct {
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	IP        string `json:"ip"`
	CreatedAt string `json:"created_at"`
}

func (h *Handlers) Active(w http.ResponseWriter, r *http.Request) {
	var req Request
	json.NewDecoder(r.Body).Decode(&req)
	var selector string
	if req.TaskName == "" {
		selector = fmt.Sprintf("client=%s", req.ClientName)
	} else {
		selector = fmt.Sprintf("app=%s-%s", req.TaskName, req.ClientName)
	}

	pods, _ := h.api.Clientset.CoreV1().Pods("potee").List(context.TODO(),
		metav1.ListOptions{LabelSelector: selector})

	for _, pod := range pods.Items {
		resp := StatusRespone{pod.Name, string(pod.Status.Phase), pod.Status.PodIP, pod.CreationTimestamp.Format("15:04")}
		json.NewEncoder(w).Encode(&resp)
		return
	}
	json.NewEncoder(w).Encode(&StatusRespone{"", "", "", ""})
}

func (h *Handlers) Status(w http.ResponseWriter, r *http.Request) {
	var req Request
	json.NewDecoder(r.Body).Decode(&req)
	selector := fmt.Sprintf("app=%s-%s", req.TaskName, req.ClientName)
	pods, _ := h.api.Clientset.CoreV1().Pods("potee").List(context.TODO(),
		metav1.ListOptions{LabelSelector: selector})

	for _, pod := range pods.Items {
		resp := StatusRespone{pod.Name, string(pod.Status.Phase), pod.Status.PodIP, pod.CreationTimestamp.Format("15:04")}

		json.NewEncoder(w).Encode(&resp)
		return
	}

}

type RequestLogger struct {
	h http.Handler
	l *log.Logger
}

func (rl RequestLogger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// rl.l.Printf("Started %s %s", r.Method, r.URL.Path)
	rl.h.ServeHTTP(w, r)
	rl.l.Printf("- %v - %s %s in %v", start.Format("2006-01-02T15:04:05Z07:00"), r.Method, r.URL.Path, time.Since(start))
}

func Router() *http.ServeMux {

	h := Handlers{api: InitK8sApi(kubeconfig)}
	mux := http.NewServeMux()
	runHandler := http.HandlerFunc(h.Run)
	mux.Handle("/run", Auth(runHandler))

	stopHandler := http.HandlerFunc(h.Stop)
	mux.Handle("/stop", Auth(stopHandler))

	activeHandler := http.HandlerFunc(h.Active)
	mux.Handle("/active", Auth(activeHandler))

	statusHandler := http.HandlerFunc(h.Status)
	mux.Handle("/status", Auth(statusHandler))

	return mux
}

var podsTimer = InitTimer()

func main() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "")
	flag.StringVar(&filename, "f", "", "")
	flag.StringVar(&command, "c", "", "")
	flag.Parse()
	l := log.New(os.Stdout, "app ", 0)
	log.Print("Listening...")
	log.Fatalln(http.ListenAndServe(":3000", RequestLogger{Router(), l}))

}
