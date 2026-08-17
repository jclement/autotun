#!/usr/bin/env python3
"""A minimal HTTP server that identifies which port it is serving.

The e2e suite forwards several of these at once and then checks that each local
port reaches the matching remote one, which is what actually proves the tunnels
are not crossed.
"""
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(sys.argv[1])
BIND = sys.argv[2] if len(sys.argv) > 2 else "127.0.0.1"


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = f"autotun-devserver port={PORT}\n".encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    HTTPServer((BIND, PORT), Handler).serve_forever()
