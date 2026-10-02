#!/usr/bin/env python3
# Minimal static file server with HTTP Range support, for blkmap's e2e script (python's
# stock http.server answers Range requests with 200 + the whole body, which blkmap rejects).
# Usage: rangehttpd.py DIR PORT [BYTES_LOG]   (BYTES_LOG receives one line per response: bytes sent)
import os
import sys
from http.server import SimpleHTTPRequestHandler, HTTPServer


class RangeHandler(SimpleHTTPRequestHandler):
    def send_head(self):
        path = self.translate_path(self.path)
        if not os.path.isfile(path):
            return super().send_head()
        size = os.path.getsize(path)
        rng = self.headers.get("Range")
        if not rng or not rng.startswith("bytes="):
            self.send_response(200)
            self.send_header("Content-Length", str(size))
            self.send_header("Accept-Ranges", "bytes")
            self.end_headers()
            return open(path, "rb")
        start, end = rng[6:].split("-")
        start = int(start)
        end = int(end) if end else size - 1
        end = min(end, size - 1)
        f = open(path, "rb")
        f.seek(start)
        self.send_response(206)
        self.send_header("Content-Range", "bytes %d-%d/%d" % (start, end, size))
        self.send_header("Content-Length", str(end - start + 1))
        self.send_header("Accept-Ranges", "bytes")
        self.end_headers()
        self._limit = end - start + 1
        return f

    def copyfile(self, source, outputfile):
        limit = getattr(self, "_limit", None)
        if limit is None:
            return super().copyfile(source, outputfile)
        data = source.read(limit)
        outputfile.write(data)
        if BYTES_LOG:
            with open(BYTES_LOG, "a") as f:
                f.write("%d\n" % len(data))

    def log_message(self, *args):
        pass


BYTES_LOG = sys.argv[3] if len(sys.argv) > 3 else None

if __name__ == "__main__":
    os.chdir(sys.argv[1])
    HTTPServer(("127.0.0.1", int(sys.argv[2])), RangeHandler).serve_forever()
