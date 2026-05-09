# Vendored static assets

These files are served by the embedded HTTP dashboard via `embed.FS`.

| File              | Source                                                        | Version |
| ----------------- | ------------------------------------------------------------- | ------- |
| `htmx.min.js`     | https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js             | 2.0.4   |
| `htmx-ws.min.js`  | https://unpkg.com/htmx-ext-ws@2.0.2/ws.js                     | 2.0.2   |
| `dashboard.css`   | `elixir/priv/static/dashboard.css` (vendored verbatim, v0)    | n/a     |

To refresh:

```sh
curl -fsSL -o htmx.min.js     https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js
curl -fsSL -o htmx-ws.min.js  https://unpkg.com/htmx-ext-ws@2.0.2/ws.js
```
