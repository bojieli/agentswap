# The README demo

The demo is the small, deterministic companion to the user-facing
[README](../README.md). It shows the account-failover path without spending a
real account's quota. It is not a substitute for the
[acceptance record](../docs/acceptance.md), which covers the full proxy,
supervisor, provider, and session-transfer behavior.

`docs/demo.svg` is generated, not drawn. Regenerate it after any change that
alters what those commands print:

```sh
./demo/demo.sh > /tmp/demo.out
./demo/render.py < /tmp/demo.out > docs/demo.svg
```

`demo.sh` builds the current binary, starts a real daemon with two pooled
accounts, and sends a real request through it. The only stand-in is the
upstream, which refuses the first account with a 429 — the demo has to show an
account being refused on purpose, and that is not something to arrange against
somebody's real quota. Every line in the image is that session's actual output.

`render.py` only colours and typesets: it never invents text. A static frame
rather than an animation, so the image renders the same way everywhere,
including where animation is stripped.

If you would rather have a GIF, `demo.sh` is an ordinary script and works under
[vhs](https://github.com/charmbracelet/vhs) or
[asciinema](https://asciinema.org) unchanged.

`docs/social-preview.png` is the image GitHub shows when the repo is linked.
GitHub's API cannot set it, so it is uploaded by hand in
Settings → General → Social preview.
