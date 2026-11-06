// h3upstream: disposable HTTP upstream for benchmarking.
//
// Standalone on purpose: it does not touch Xray or any production service.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

func main() {
	var (
		addr = flag.String("addr", "unix:/tmp/h3up.sock", "unix:/path or host:port")
		size = flag.String("size", "2G", "body size: 256M / 2G")
	)
	flag.Parse()

	n, err := parseSize(*size)
	if err != nil {
		log.Fatalf("bad -size: %v", err)
	}

	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = 'a' + byte(i%26)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		w.WriteHeader(200)

		var sent int64
		for sent < n {
			m := int64(len(chunk))
			if n-sent < m {
				m = n - sent
			}
			k, err := w.Write(chunk[:m])
			if err != nil {
				return
			}
			sent += int64(k)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			default:
			}
		}
	})

	mux.HandleFunc("/tiny", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	var ln net.Listener
	if strings.HasPrefix(*addr, "unix:") {
		path := strings.TrimPrefix(*addr, "unix:")
		os.Remove(path)
		ln, err = net.Listen("unix", path)
		if err == nil {
			os.Chmod(path, 0777)
		}
	} else {
		ln, err = net.Listen("tcp", *addr)
	}
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	log.Printf("h3upstream on %s, body=%d", *addr, n)
	log.Fatal(http.Serve(ln, mux))
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, s[:len(s)-1]
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, s[:len(s)-1]
	case strings.HasSuffix(s, "K"):
		mult, s = 1<<10, s[:len(s)-1]
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", s, err)
	}
	return v * mult, nil
}
