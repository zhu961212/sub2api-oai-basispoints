package main

import (
	"os"
	"os/signal"
	"syscall"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/transport"
)

func main() {
	t := transport.New()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-stop; t.Shutdown() }()
	pluginv1.Serve(t)
}
