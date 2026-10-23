package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestGateWaitsAndRejectsLateHandlers(t *testing.T) {
	entered, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	gate := &requestGate{next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-resume; w.WriteHeader(204) })}
	first := httptest.NewRecorder()
	go func() { gate.ServeHTTP(first, httptest.NewRequest("GET", "/", nil)); close(finished) }()
	<-entered
	drained := gate.finish()
	late := httptest.NewRecorder()
	gate.ServeHTTP(late, httptest.NewRequest("GET", "/", nil))
	if late.Code != 503 {
		t.Errorf("late handler admitted: %d", late.Code)
	}
	select {
	case <-drained:
		t.Error("gate drained with an active handler")
	default:
	}
	close(resume)
	<-finished
	<-drained
	if first.Code != 204 {
		t.Fatal("admitted request was not allowed to finish")
	}
}
