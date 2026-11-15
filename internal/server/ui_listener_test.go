package server

import (
	"net"
	"net/http"
	"testing"
	"time"
)

func TestUIServedOnSeparateListener(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	uln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.UIListener = uln
	s.UIHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		text(w, http.StatusTeapot, "ui "+r.URL.Path)
	})
	r := f.start(s)
	base := "http://" + uln.Addr().String()
	if code, body := status(t, base+"/anything"); code != http.StatusTeapot || body != "ui /anything\n" {
		t.Fatalf("ui listener: %d %q", code, body)
	}
	if code, _ := status(t, base+"/healthz"); code != http.StatusTeapot {
		t.Fatalf("ui listener /healthz should reach the UI handler, got %d", code)
	}
	if code, _ := status(t, "http://"+s.Addr().String()+"/anything"); code != http.StatusNotFound {
		t.Fatalf("main listener served the UI: %d", code)
	}
	if err := r.stop(t, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", uln.Addr().String(), time.Second); err == nil {
		c.Close()
		t.Fatal("ui listener still accepting after shutdown")
	}
}

func TestUIListenerWithoutHandlerFails(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	uln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer uln.Close()
	s.UIListener = uln
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := s.Run(t.Context(), ln); err == nil {
		t.Fatal("Run accepted a UIListener without UIHandler")
	}
}
