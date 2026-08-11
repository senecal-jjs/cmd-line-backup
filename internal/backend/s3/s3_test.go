package s3

import (
	"testing"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
)

func TestParseURL(t *testing.T) {
	tests := []struct {
		raw        string
		wantBucket string
		wantPrefix string
		wantErr    bool
	}{
		{"s3://my-bucket", "my-bucket", "", false},
		{"s3://my-bucket/", "my-bucket", "", false},
		{"s3://my-bucket/backups", "my-bucket", "backups", false},
		{"s3://my-bucket/backups/", "my-bucket", "backups", false},
		{"s3://my-bucket/team/laptop", "my-bucket", "team/laptop", false},
		{"https://my-bucket", "", "", true},
		{"s3://", "", "", true},
		{"my-bucket", "", "", true},
		{"", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			cfg, err := ParseURL(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseURL(%q) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if cfg.Bucket != tt.wantBucket {
				t.Errorf("Bucket = %q, want %q", cfg.Bucket, tt.wantBucket)
			}
			if cfg.Prefix != tt.wantPrefix {
				t.Errorf("Prefix = %q, want %q", cfg.Prefix, tt.wantPrefix)
			}
		})
	}
}

func TestRangeHeader(t *testing.T) {
	tests := []struct {
		offset, length int64
		want           string
	}{
		{0, 100, "bytes=0-99"},
		{50, 10, "bytes=50-59"},
		{0, 1, "bytes=0-0"},
		{100, 0, "bytes=100-"},
		{0, 0, "bytes=0-"},
	}
	for _, tt := range tests {
		if got := rangeHeader(tt.offset, tt.length); got != tt.want {
			t.Errorf("rangeHeader(%d, %d) = %q, want %q", tt.offset, tt.length, got, tt.want)
		}
	}
}

func TestKeyIncludesPrefix(t *testing.T) {
	tests := []struct {
		prefix string
		handle backend.Handle
		want   string
	}{
		{"", backend.Handle{Type: backend.TypeConfig}, "config"},
		{"backups", backend.Handle{Type: backend.TypeConfig}, "backups/config"},
		{"", backend.Handle{Type: backend.TypePack, Name: "ab1234"}, "packs/ab/ab1234"},
		{"backups", backend.Handle{Type: backend.TypePack, Name: "ab1234"}, "backups/packs/ab/ab1234"},
		{"a/b", backend.Handle{Type: backend.TypeSnapshot, Name: "snap1"}, "a/b/snapshots/snap1"},
	}
	for _, tt := range tests {
		b := &Backend{cfg: Config{Bucket: "bucket", Prefix: tt.prefix}}
		if got := b.key(tt.handle); got != tt.want {
			t.Errorf("key(%v) with prefix %q = %q, want %q", tt.handle, tt.prefix, got, tt.want)
		}
	}
}

func TestLocation(t *testing.T) {
	tests := []struct {
		cfg  Config
		want string
	}{
		{Config{Bucket: "b"}, "s3://b"},
		{Config{Bucket: "b", Prefix: "p"}, "s3://b/p"},
		{Config{Bucket: "b", Prefix: "p/q"}, "s3://b/p/q"},
	}
	for _, tt := range tests {
		b := &Backend{cfg: tt.cfg}
		if got := b.Location(); got != tt.want {
			t.Errorf("Location() = %q, want %q", got, tt.want)
		}
	}
}

func TestOpenRequiresBucket(t *testing.T) {
	if _, err := Open(t.Context(), Config{}); err == nil {
		t.Error("Open succeeded without a bucket name")
	}
}
