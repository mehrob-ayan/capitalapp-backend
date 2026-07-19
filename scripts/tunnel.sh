#!/bin/sh
# Public HTTPS tunnel to the local backend (port 8080).
# Pick ONE tunnel below and edit its details, then leave the rest commented.
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"

# --- ngrok (static domain; no own domain needed) ---
exec ngrok http --domain=CHANGE-ME.ngrok-free.app 8080

# --- Cloudflare Tunnel (needs your domain on Cloudflare) ---
# exec cloudflared tunnel run capital
