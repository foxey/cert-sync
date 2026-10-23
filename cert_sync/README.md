# cert-sync (Home Assistant App)

Receives an ACME certificate pushed by a [cert-sync](https://github.com/foxey/cert-sync)
server over Redis and installs it for Home Assistant Core's `http:` integration —
no Traefik involved on this side.

Because the certificate is a wildcard issued for the Traefik deployment (e.g.
`*.app.example.com`), Home Assistant's `base_url` must use a hostname that
matches it.

See `DOCS.md` for configuration.
