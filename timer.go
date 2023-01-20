package main

import (
	"log"
	"os"
	"strconv"
	"time"
)

type PodsTimer struct {
	LifeTime int
	Timers   map[string]*time.Timer
}

func Getenv(key, defaultValue string) string {
	if env := os.Getenv(key); env == "" {
		return defaultValue
	} else {
		return env
	}
}

func InitTimer() *PodsTimer {
	var podsTimer PodsTimer
	var err error

	podsTimer.LifeTime, err = strconv.Atoi(Getenv("LIFETIME", "1"))
	if err != nil {
		log.Fatalf("can't conver lifetime value to int: %v\n", err)
		return &podsTimer
	}
	log.Println("pod liftime", podsTimer.LifeTime)
	podsTimer.Timers = make(map[string]*time.Timer)
	return &podsTimer
}

func (t *PodsTimer) CreatePodTimer(podName string, destroyFunc func()) {
	t.Timers[podName] = time.AfterFunc(time.Duration(t.LifeTime)*time.Minute, destroyFunc)
}

func (t *PodsTimer) StopPodTimer(podName string) {
	timer, ok := t.Timers[podName]
	if ok {
		timer.Stop()
		log.Printf("%s timer stopped", podName)
	} else {
		log.Printf("%s timer not exists", podName)
	}
}
