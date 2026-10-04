package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/proxy"
)

const (
	pubKey     = "demo"
	subKey     = "demo"
	channel    = "Cookies"
	difficulty = 5
	mobileUA   = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
)

type hashMessage struct {
	Hash  string `json:"hash"`
	Nonce string `json:"nonce"`
	UUID  string `json:"uuid"`
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func mine(uuid string) hashMessage {
	var seed [8]byte
	if _, err := rand.Read(seed[:]); err != nil {
		panic(err)
	}
	seedU := binary.BigEndian.Uint64(seed[:])

	var raw [16]byte
	var buf [32 + 36]byte
	copy(buf[32:], uuid)

	for c := uint64(0); ; c++ {
		binary.BigEndian.PutUint64(raw[0:8], seedU)
		binary.BigEndian.PutUint64(raw[8:16], c)
		hex.Encode(buf[:32], raw[:])
		sum := sha256.Sum256(buf[:])
		if sum[0] == 0 && sum[1] == 0 && sum[2]&0xf0 == 0 {
			return hashMessage{
				Hash:  hex.EncodeToString(sum[:]),
				Nonce: string(buf[:32]),
				UUID:  uuid,
			}
		}
	}
}

func newClient(torAddr string, workerID int) (*http.Client, error) {
	tr := &http.Transport{
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 256,
		IdleConnTimeout:     90 * time.Second,
	}
	if torAddr != "" {
		auth := &proxy.Auth{User: fmt.Sprintf("worker-%d", workerID), Password: "x"}
		dialer, err := proxy.SOCKS5("tcp", torAddr, auth, proxy.Direct)
		if err != nil {
			return nil, err
		}
		cd, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("socks5 dialer lacks DialContext")
		}
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return cd.DialContext(ctx, network, addr)
		}
		tr.MaxIdleConnsPerHost = 4
		return &http.Client{Transport: tr, Timeout: 120 * time.Second}, nil
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second}, nil
}

func publish(client *http.Client, ua string, msg hashMessage) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	u := fmt.Sprintf("https://ps.pndsn.com/publish/%s/%s/0/%s/0/%s",
		pubKey, subKey, channel, url.PathEscape(string(payload)))
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", ua)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, body)
			continue
		}
		return nil
	}
	return lastErr
}

func main() {
	users := flag.Int("users", 100, "number of UUID users to create")
	clicks := flag.Int("clicks", 0, "clicks per user (0 = infinite, Ctrl+C to stop)")
	pubWorkers := flag.Int("pubworkers", 64, "concurrent publish workers")
	torFlag := flag.String("tor", "auto", `Tor mode: "auto" (launch Tor if needed), "off" (direct), or a SOCKS5 address like 127.0.0.1:9050`)
	ua := flag.String("ua", mobileUA, "User-Agent header")
	flag.Parse()

	infinite := *clicks <= 0
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	torAddr := ""
	switch *torFlag {
	case "off", "":
	case "auto":
		addr, cleanup, err := ensureTor()
		if err != nil {
			fmt.Println("tor setup:", err)
			return
		}
		defer cleanup()
		torAddr = addr
	default:
		torAddr = *torFlag
	}

	if torAddr != "" {
		fmt.Println("Routing via Tor SOCKS5 at", torAddr)
		if *pubWorkers > 16 {
			*pubWorkers = 16
			fmt.Println("Tor mode: limiting publish workers to 16")
		}
	}

	total := *users * *clicks
	jobs := make(chan string, runtime.NumCPU()*2)
	mined := make(chan hashMessage, *pubWorkers*2)

	var mineWG, pubWG sync.WaitGroup
	var okCount, failCount atomic.Int64

	miners := runtime.NumCPU()
	for i := 0; i < miners; i++ {
		mineWG.Add(1)
		go func() {
			defer mineWG.Done()
			for uuid := range jobs {
				mined <- mine(uuid)
			}
		}()
	}

	for i := 0; i < *pubWorkers; i++ {
		client, err := newClient(torAddr, i)
		if err != nil {
			fmt.Println("client setup:", err)
			return
		}
		pubWG.Add(1)
		go func(client *http.Client) {
			defer pubWG.Done()
			for msg := range mined {
				if err := publish(client, *ua, msg); err != nil {
					failCount.Add(1)
					fmt.Printf("FAILED uuid=%s: %v\n", msg.UUID, err)
					continue
				}
				n := okCount.Add(1)
				if infinite {
					fmt.Printf("[%d] uuid=%s hash=%s\n", n, msg.UUID, msg.Hash)
				} else {
					fmt.Printf("[%d/%d] uuid=%s hash=%s\n", n, total, msg.UUID, msg.Hash)
				}
			}
		}(client)
	}

	start := time.Now()
	uuids := make([]string, *users)
	for i := range uuids {
		uuids[i] = newUUID()
	}

	if infinite {
		fmt.Printf("Infinite mode: %d users clicking, Ctrl+C to stop\n", *users)
		go func() {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					el := time.Since(start)
					fmt.Printf("--- stats: %d clicks, %d failed, %.1f clicks/sec avg ---\n",
						okCount.Load(), failCount.Load(), float64(okCount.Load())/el.Seconds())
				}
			}
		}()
	}

feed:
	for {
		for _, uuid := range uuids {
			rounds := *clicks
			if infinite {
				rounds = 1
			}
			for c := 0; c < rounds; c++ {
				select {
				case <-ctx.Done():
					break feed
				case jobs <- uuid:
				}
			}
		}
		if !infinite {
			break
		}
	}
	close(jobs)
	mineWG.Wait()
	close(mined)
	pubWG.Wait()

	elapsed := time.Since(start)
	if infinite {
		fmt.Printf("\nStopped after %s: %d clicks published, %d failed (%.1f clicks/sec)\n",
			elapsed.Round(time.Millisecond), okCount.Load(), failCount.Load(),
			float64(okCount.Load())/elapsed.Seconds())
	} else {
		fmt.Printf("\nDone in %s: %d/%d clicks published, %d failed (%.1f clicks/sec)\n",
			elapsed.Round(time.Millisecond), okCount.Load(), total, failCount.Load(),
			float64(okCount.Load())/elapsed.Seconds())
	}
}
