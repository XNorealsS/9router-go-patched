package updater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		v1   string
		v2   string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.1.0", "1.0.0", 1},
		{"1.0.0", "1.1.0", -1},
		{"2.0.0", "1.9.9", 1},
		{"1.0.1", "1.0.0", 1},
		{"v1.2.3", "1.2.3", 0},
		{"v1.2.4", "v1.2.3", 1},
	}

	for _, tt := range tests {
		got := CompareVersions(tt.v1, tt.v2)
		if got != tt.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.v1, tt.v2, got, tt.want)
		}
	}
}

func TestGetCachedInfo(t *testing.T) {
	info := GetCachedInfo()
	if info == nil {
		t.Fatal("expected non-nil UpdateInfo")
	}
	if info.CurrentVersion == "" {
		t.Error("expected non-empty CurrentVersion")
	}
}

func TestPerformSelfUpdateVerified_RejectsInsecureAndMissingChecksum(t *testing.T) {
	if err := PerformSelfUpdateVerified("http://example.com/bin", "deadbeef"); err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("expected https:// rejection for http download URL, got %v", err)
	}
	if err := PerformSelfUpdateVerified("https://127.0.0.1/bin", "deadbeef"); err == nil {
		t.Fatalf("expected SSRF rejection for https://127.0.0.1")
	}
	if err := PerformSelfUpdateVerified("https://8.8.8.8/bin", ""); err == nil || !strings.Contains(err.Error(), "ALLOW_UNVERIFIED_UPDATE") {
		t.Fatalf("expected missing checksum rejection, got %v", err)
	}
}

func TestPerformSelfUpdateVerified_RejectsChecksumMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fake-binary-bytes"))
	}))
	defer srv.Close()

	t.Setenv("ALLOW_INSECURE_UPDATE", "true")
	err := PerformSelfUpdateVerified(srv.URL, "0000000000000000000000000000000000000000000000000000000000000000")
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("expected SHA-256 mismatch error, got %v", err)
	}
}

func TestCheckUpdate_SupportsLatestVersionAndSHA256(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"latestVersion":"9.9.9","downloadUrl":"https://example.com/9router-go","sha256":"ABCD1234"}`))
	}))
	defer srv.Close()

	t.Setenv("ALLOW_INSECURE_UPDATE", "true")
	t.Setenv("UPDATE_URL", srv.URL)
	info, err := CheckUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckUpdate failed: %v", err)
	}
	if !info.HasUpdate || info.LatestVersion != "9.9.9" || info.DownloadURL != "https://example.com/9router-go" || info.SHA256 != "abcd1234" {
		t.Fatalf("unexpected UpdateInfo: %+v", info)
	}
}
