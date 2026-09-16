package http

import (
	"html"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	apispec "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/api"
)

// AsyncAPIYAMLHandler serves the raw AsyncAPI 3.0 spec (§7.3).
func AsyncAPIYAMLHandler(c *gin.Context) {
	c.Data(http.StatusOK, "application/yaml; charset=utf-8", apispec.AsyncAPISpec)
}

// AsyncAPIHandler renders the embedded spec as a plain, readable HTML page
// — this service's event contract is 5 published messages on one topic
// (unlike the sibling Realm Provisioner's multi-producer/multi-queue
// contract), so a full interactive catalog UI is not warranted; a styled
// <pre> of the source-of-truth YAML is sufficient and never drifts from
// api/asyncapi.yaml (§7.3, §12.1).
func AsyncAPIHandler(c *gin.Context) {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>iam-token-service — AsyncAPI</title>
<style>
:root{color-scheme:dark light}
body{margin:0;padding:2rem;background:#0d0d0d;color:#e5e7eb;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
h1{font-size:1.1rem;color:#fff;margin:0 0 1rem}
p{color:#9ca3af;font-size:.85rem;margin:0 0 1.5rem}
a{color:#e879f9}
pre{background:#050505;border:1px solid rgba(132,38,176,.3);border-radius:8px;padding:1.25rem;overflow-x:auto;font-size:.82rem;line-height:1.5;white-space:pre-wrap}
</style>
</head>
<body>
<h1>iam-token-service — AsyncAPI 3.0</h1>
<p>iam.serviceaccount.events — 5 published events, audit-only (TS-INV-4). Raw spec: <a href="/asyncapi.yaml">/asyncapi.yaml</a></p>
<pre>`)
	b.WriteString(html.EscapeString(string(apispec.AsyncAPISpec)))
	b.WriteString(`</pre>
</body>
</html>`)

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, b.String())
}
