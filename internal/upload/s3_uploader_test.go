package upload

import "testing"

func TestNewS3Uploader_RejectsIncompleteConfig(t *testing.T) {
	_, err := NewS3Uploader(S3Config{
		Endpoint: "s3.example.com",
		Bucket:   "binlog",
	})
	if err == nil {
		t.Fatal("expected error for incomplete config")
	}
}
