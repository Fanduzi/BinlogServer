// Package upload proves object-storage TLS against a private CA.
// input: BINLOG_TEST_TLS=1, openssl, and a MinIO process (docker image or the GitHub release binary)
// output: one sealed object uploaded over HTTPS with upload.tls.ca_file, a failure when that CA is not trusted, and a plain-HTTP MinIO that tells the operator to set upload.use_ssl: false
// pos: the CI e2e-tls proof that dropping the S3 TLS transport goes red
// note: if this file changes, update this header and module README.md.
package upload

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/livemysql"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const minioRelease = "RELEASE.2025-09-07T16-13-09Z"

func TestLiveS3TLS(t *testing.T) {
	livemysql.Require(t)
	dir := t.TempDir()
	livemysql.WriteCerts(t, dir, "subjectAltName=IP:127.0.0.1,DNS:localhost\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\n")
	certs := filepath.Join(dir, "certs")
	if err := os.MkdirAll(certs, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join(dir, "server.pem"), filepath.Join(certs, "public.crt"))
	copyFile(t, filepath.Join(dir, "server.key"), filepath.Join(certs, "private.key"))
	port := livemysql.FreePort(t)
	startMinIO(t, port, certs, filepath.Join(dir, "data"))
	waitMinIO(t, port)
	ca := filepath.Join(dir, "ca.pem")
	wrong := filepath.Join(dir, "wrong-ca.pem")
	endpoint := fmt.Sprintf("127.0.0.1:%d", port)
	makeBucket(t, endpoint, ca)

	key := "g2/uuid/mysql-bin.000001"
	local := filepath.Join(dir, "mysql-bin.000001")
	if err := os.WriteFile(local, []byte("sealed-segment"), 0o644); err != nil {
		t.Fatal(err)
	}
	up := newLiveUploader(t, endpoint, ca)
	if err := up.UploadFile(context.Background(), "g2", local, key); err != nil {
		t.Fatalf("https upload: %v", err)
	}
	if err := newLiveUploader(t, endpoint, wrong).UploadFile(context.Background(), "g2", local, key); err == nil || !TLSTransportFailure(err.Error()) {
		t.Fatalf("wrong CA = %v", err)
	}
	if err := newLiveUploader(t, endpoint, "").UploadFile(context.Background(), "g2", local, key+"-plain"); err == nil || !TLSTransportFailure(err.Error()) {
		t.Fatalf("system roots = %v", err)
	}

	t.Run("plain-http", func(t *testing.T) {
		plainPort := livemysql.FreePort(t)
		plainData := filepath.Join(dir, "plain-data")
		startPlainMinIO(t, plainPort, plainData)
		waitPlainMinIO(t, plainPort)
		plainEndpoint := fmt.Sprintf("127.0.0.1:%d", plainPort)
		makePlainBucket(t, plainEndpoint)
		up, err := NewS3Uploader(S3Config{
			Endpoint: plainEndpoint, Bucket: "binlog", AccessKey: "minioadmin", SecretKey: "minioadmin", UseSSL: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = up.ProbeStorage(ctx)
		if err == nil || !plainHTTPEndpoint(err.Error()) || !strings.Contains(err.Error(), "upload.use_ssl: false") || strings.Contains(err.Error(), "upload.tls.ca_file") {
			t.Fatalf("plain HTTP MinIO = %v", err)
		}
	})
}

func newLiveUploader(t *testing.T, endpoint, ca string) *S3Uploader {
	t.Helper()
	up, err := NewS3Uploader(S3Config{
		Endpoint: endpoint, Bucket: "binlog", AccessKey: "minioadmin", SecretKey: "minioadmin",
		UseSSL: true, CAFile: ca,
	})
	if err != nil {
		t.Fatal(err)
	}
	return up
}

func makeBucket(t *testing.T, endpoint, ca string) {
	t.Helper()
	pool, err := loadCAFile(ca)
	if err != nil {
		t.Fatal(err)
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds: credentials.NewStaticV4("minioadmin", "minioadmin", ""), Secure: true,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.MakeBucket(ctx, "binlog", minio.MakeBucketOptions{}); err != nil {
		resp := minio.ToErrorResponse(err)
		if resp.Code != "BucketAlreadyOwnedByYou" && resp.Code != "BucketAlreadyExists" {
			t.Fatal(err)
		}
	}
}

func startMinIO(t *testing.T, port int, certs, data string) {
	t.Helper()
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	image := "quay.io/minio/minio:" + minioRelease
	if exec.Command("docker", "image", "inspect", image).Run() != nil {
		if out, err := exec.Command("docker", "pull", image).CombinedOutput(); err != nil {
			t.Logf("minio image unavailable (%v); using the release binary\n%s", err, out)
			startMinIOBinary(t, port, certs, data)
			return
		}
	}
	name := "binlog-g2-minio"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	out, err := exec.Command("docker", "run", "-d", "--name", name,
		"-e", "MINIO_ROOT_USER=minioadmin", "-e", "MINIO_ROOT_PASSWORD=minioadmin",
		"-p", fmt.Sprintf("127.0.0.1:%d:9000", port),
		"-v", certs+":/certs:ro", "-v", data+":/data",
		image, "server", "/data", "--address", ":9000", "--certs-dir", "/certs",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run minio: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
}

func startPlainMinIO(t *testing.T, port int, data string) {
	t.Helper()
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := minioBinary(t)
	cmd := exec.Command(bin, "server", data, "--address", fmt.Sprintf("127.0.0.1:%d", port))
	cmd.Env = append(os.Environ(), "MINIO_ROOT_USER=minioadmin", "MINIO_ROOT_PASSWORD=minioadmin")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
}

func waitPlainMinIO(t *testing.T, port int) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/minio/health/live", port)
	deadline := time.Now().Add(40 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("plain minio not ready: %v", last)
}

func makePlainBucket(t *testing.T, endpoint string) {
	t.Helper()
	client, err := minio.New(endpoint, &minio.Options{
		Creds: credentials.NewStaticV4("minioadmin", "minioadmin", ""), Secure: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.MakeBucket(ctx, "binlog", minio.MakeBucketOptions{}); err != nil {
		resp := minio.ToErrorResponse(err)
		if resp.Code != "BucketAlreadyOwnedByYou" && resp.Code != "BucketAlreadyExists" {
			t.Fatal(err)
		}
	}
}

func startMinIOBinary(t *testing.T, port int, certs, data string) {
	t.Helper()
	bin := minioBinary(t)
	cmd := exec.Command(bin, "server", data, "--address", fmt.Sprintf("127.0.0.1:%d", port), "--certs-dir", certs)
	cmd.Env = append(os.Environ(), "MINIO_ROOT_USER=minioadmin", "MINIO_ROOT_PASSWORD=minioadmin")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
}

func minioBinary(t *testing.T) string {
	t.Helper()
	arch := runtime.GOARCH
	switch arch {
	case "amd64", "arm64":
	default:
		t.Fatalf("no minio binary for %s", arch)
	}
	dir := "/tmp/binlog-e2e-minio-bins"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "minio-"+minioRelease)
	if st, err := os.Stat(dest); err == nil && st.Mode()&0o111 != 0 {
		return dest
	}
	url := fmt.Sprintf("https://github.com/minio/minio/releases/download/%s/minio.linux-%s.%s", minioRelease, arch, minioRelease)
	out, err := exec.Command("curl", "-fsSL", "-o", dest, url).CombinedOutput()
	if err != nil {
		t.Fatalf("download minio: %v\n%s", err, out)
	}
	if err := os.Chmod(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	return dest
}

func waitMinIO(t *testing.T, port int) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	url := fmt.Sprintf("https://127.0.0.1:%d/minio/health/live", port)
	deadline := time.Now().Add(40 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("minio not ready: %v", last)
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, in, 0o600); err != nil {
		t.Fatal(err)
	}
}
