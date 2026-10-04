package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const torIndexURL = "https://archive.torproject.org/tor-package-archive/torbrowser/"

func portOpen(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func findTorBinary() string {
	if p, err := exec.LookPath("tor.exe"); err == nil {
		return p
	}
	if p, err := exec.LookPath("tor"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join("tor", "tor.exe"),
		filepath.Join(home, "Desktop", "Tor Browser", "Browser", "TorBrowser", "Tor", "tor.exe"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Tor Browser", "Browser", "TorBrowser", "Tor", "tor.exe"),
		filepath.Join(os.Getenv("PROGRAMFILES"), "Tor Browser", "Browser", "TorBrowser", "Tor", "tor.exe"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

type version []int

func parseVersion(s string) version {
	parts := strings.Split(s, ".")
	v := make(version, len(parts))
	for i, p := range parts {
		v[i], _ = strconv.Atoi(p)
	}
	return v
}

func (a version) less(b version) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

func latestTorURLs(client *http.Client) ([]string, error) {
	resp, err := client.Get(torIndexURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(`href="([0-9]+(?:\.[0-9]+)+)/"`)
	seen := map[string]bool{}
	var versions []version
	for _, m := range re.FindAllStringSubmatch(string(body), -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		versions = append(versions, parseVersion(m[1]))
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("no versions found at %s", torIndexURL)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[j].less(versions[i]) })
	var urls []string
	for _, v := range versions {
		vs := make([]string, len(v))
		for i, n := range v {
			vs[i] = strconv.Itoa(n)
		}
		s := strings.Join(vs, ".")
		urls = append(urls, torIndexURL+s+"/tor-expert-bundle-windows-x86_64-"+s+".tar.gz")
	}
	return urls, nil
}

func downloadTor(client *http.Client, destDir string) (string, error) {
	urls, err := latestTorURLs(client)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	for _, u := range urls[:5] {
		fmt.Println("Downloading Tor:", u)
		resp, err := client.Get(u)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}
		err = extractBundle(resp.Body, destDir)
		resp.Body.Close()
		if err != nil {
			return "", err
		}
		torPath := filepath.Join(destDir, "tor.exe")
		if _, err := os.Stat(torPath); err == nil {
			return torPath, nil
		}
	}
	return "", fmt.Errorf("failed to download Tor expert bundle")
}

func extractBundle(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := strings.ToLower(filepath.Base(hdr.Name))
		if !strings.HasSuffix(name, ".exe") && !strings.HasSuffix(name, ".dll") {
			continue
		}
		out, err := os.Create(filepath.Join(destDir, name))
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		out.Close()
	}
}

func ensureTor() (string, func(), error) {
	const socksAddr = "127.0.0.1:9050"
	if portOpen(socksAddr) {
		fmt.Println("Tor already running at", socksAddr)
		return socksAddr, func() {}, nil
	}

	torPath := findTorBinary()
	if torPath == "" {
		dlClient := &http.Client{Timeout: 10 * time.Minute}
		var err error
		torPath, err = downloadTor(dlClient, "tor")
		if err != nil {
			return "", nil, err
		}
	}
	fmt.Println("Starting Tor:", torPath)

	dataDir := filepath.Join(filepath.Dir(torPath), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", nil, err
	}
	cmd := exec.Command(torPath,
		"--SocksPort", socksAddr,
		"--DataDirectory", dataDir,
		"--Log", "err stdout",
	)
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	cleanup := func() {
		cmd.Process.Kill()
		cmd.Wait()
	}

	fmt.Print("Waiting for Tor bootstrap")
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if portOpen(socksAddr) {
			fmt.Println(" ready")
			return socksAddr, cleanup, nil
		}
		fmt.Print(".")
		time.Sleep(2 * time.Second)
	}
	cleanup()
	return "", nil, fmt.Errorf("Tor failed to start within 120s")
}
