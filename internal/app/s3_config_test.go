// Package app proves the object client receives upload.tls.ca_file.
// input: a Config with UploadCAFile set
// output: the same path on the S3Config the process starts with
// pos: unit coverage for the upload.tls.ca_file wiring
// note: if this file changes, update this header and module README.md.
package app

import (
	"testing"

	"binlog_server/internal/config"
)

func TestS3ConfigFromCopiesCAFile(t *testing.T) {
	got := s3ConfigFrom(config.Config{
		UploadEndpoint: "127.0.0.1:9000",
		UploadBucket:   "binlog",
		UploadUseSSL:   true,
		UploadCAFile:   "/etc/binlog-server/s3-ca.pem",
	})
	if got.CAFile != "/etc/binlog-server/s3-ca.pem" || !got.UseSSL || got.Endpoint != "127.0.0.1:9000" {
		t.Fatalf("%+v", got)
	}
}
