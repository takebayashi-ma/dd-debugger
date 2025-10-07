package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/vmihailenco/msgpack/v5"
)

func main() {
	pretty := strings.EqualFold(os.Getenv("LOG_PRETTY_JSON"), "true")
	quietHTTP := strings.EqualFold(os.Getenv("QUIET_HTTP_LOG"), "true")
	enableMetrics := strings.EqualFold(os.Getenv("ENABLE_METRICS"), "false")

	mux := http.NewServeMux()
	for _, p := range []string{
		"/v0.5/traces",
		"/v0.4/traces",
		"/v0.3/traces",
		"/v0.2/traces",
		"/v0.1/spans",
	} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "only POST", http.StatusMethodNotAllowed)
				return
			}
			if err := dumpTraceRequest(r, pretty); err != nil {
				log.Printf("[apm] error: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		})
	}

	var handler http.Handler = mux
	if !quietHTTP {
		handler = logMiddleware(mux)
	}

	srv := &http.Server{
		Addr:              ":8126",
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	udpStop := make(chan struct{})
	if enableMetrics {
		go runDogStatsD(":8125", udpStop)
	}

	errCh := make(chan error, 1)
	go func() {
		if !quietHTTP {
			log.Printf("[mock-agent] HTTP APM listening on %s", srv.Addr)
		}
		errCh <- srv.ListenAndServe()
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-sig:
	case err := <-errCh:
		log.Printf("[mock-agent] http server error: %v", err)
	}

	close(udpStop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[mock-agent] %s %s Content-Type=%s Content-Encoding=%s",
			r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Content-Encoding"))
		next.ServeHTTP(w, r)
	})
}

func runDogStatsD(addr string, stop <-chan struct{}) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		log.Printf("[dogstatsd] resolve error: %v", err)
		return
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Printf("[dogstatsd] listen error: %v", err)
		return
	}
	defer conn.Close()

	if !strings.EqualFold(os.Getenv("QUIET_HTTP_LOG"), "true") {
		log.Printf("[dogstatsd] listening on %s (UDP)", addr)
	}

	buf := make([]byte, 65535)
	for {
		select {
		case <-stop:
			return
		default:
			_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				log.Printf("[dogstatsd] read error: %v", err)
				continue
			}
			// DogStatsD は複数行来るので 1 行ごとに出力（そのまま）
			lines := strings.Split(string(buf[:n]), "\n")
			for _, line := range lines {
				if strings.TrimSpace(line) == "" {
					continue
				}
				os.Stdout.WriteString(line) // 1 行 = 1 メトリクス
				os.Stdout.WriteString("\n")
			}
		}
	}
}

// dumpTraceRequest は、受け取った APM ペイロードを JSON に変換して
// 1 リクエスト = 1 出力（NDJSONまたはインデント JSON）で stdout へ書き出す。
func dumpTraceRequest(r *http.Request, pretty bool) error {
	defer r.Body.Close()

	// Decompress
	var body []byte
	switch strings.ToLower(r.Header.Get("Content-Encoding")) {
	case "gzip":
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			return err
		}
		defer gr.Close()
		b, err := io.ReadAll(gr)
		if err != nil {
			return err
		}
		body = b
	case "zstd":
		dec, err := zstd.NewReader(r.Body)
		if err != nil {
			return err
		}
		defer dec.Close()
		b, err := io.ReadAll(dec)
		if err != nil {
			return err
		}
		body = b
	default:
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		body = b
	}

	ct := strings.ToLower(r.Header.Get("Content-Type"))
	var out []byte

	// dd-trace-go は application/msgpack で送るのが一般的
	if strings.Contains(ct, "application/msgpack") {
		var v interface{}
		dec := msgpack.NewDecoder(bytes.NewReader(body))
		dec.SetCustomStructTag("json")
		if err := dec.Decode(&v); err != nil {
			return err
		}
		// コンパクト or インデント
		if pretty {
			out, _ = json.MarshalIndent(v, "", "  ")
		} else {
			out, _ = json.Marshal(v)
		}
	} else {
		// 既に JSON の場合など
		if pretty && json.Valid(body) {
			var buf bytes.Buffer
			_ = json.Indent(&buf, body, "", "  ")
			out = buf.Bytes()
		} else {
			out = body
		}
	}

	// ここがポイント：**1 回の Write**で JSON を出す（ログの混入を避ける）
	os.Stdout.Write(out)
	os.Stdout.WriteString("\n")
	return nil
}
