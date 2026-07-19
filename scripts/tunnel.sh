#!/bin/sh
# Public HTTPS tunnel to the local backend (port 8080).
# Using Tailscale Funnel: stable https://<machine>.<tailnet>.ts.net, free, no
# interstitial. Requires one-time setup (see server/deploy/README.md):
#   brew install tailscale && sudo tailscaled install-system-daemon
#   tailscale up            # log in
#   tailscale funnel 8080   # first run prints a link to enable Funnel
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"

exec tailscale funnel 8080

# --- Alternatives (leave commented) ---
# exec cloudflared tunnel run capital
# exec ngrok http --domain=CHANGE-ME.ngrok-free.app 8080
