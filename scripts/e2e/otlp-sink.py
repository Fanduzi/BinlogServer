#!/usr/bin/env python3
# input: listen port and an output directory (argv); OTLP/HTTP trace exports POSTed to /v1/traces
# output: one file per export request body (protobuf, as received) in the output directory; HTTP 200 with an empty ExportTraceServiceResponse
# pos: test-only collector for smoke-observability's trace assertion (run-observability.sh starts it)
# note: if this file changes, update this header and module README.md.
import http.server
import os
import sys
import threading

PORT = int(sys.argv[1])
OUT = sys.argv[2]
os.makedirs(OUT, exist_ok=True)
lock = threading.Lock()
count = 0


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        global count
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        if self.path != "/v1/traces":
            self.send_response(404)
            self.end_headers()
            return
        with lock:
            count += 1
            n = count
        with open(os.path.join(OUT, "export-%05d.pb" % n), "wb") as f:
            f.write(body)
        self.send_response(200)
        self.send_header("Content-Type", "application/x-protobuf")
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, fmt, *args):
        pass


http.server.ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
