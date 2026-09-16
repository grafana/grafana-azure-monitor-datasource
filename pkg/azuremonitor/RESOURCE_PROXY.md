# Resource proxy authorization model

The Azure Monitor, Log Analytics, and Resource Graph resource-proxy routes
(`azuremonitor-resource-handler.go`) let the frontend browse Azure metadata —
subscriptions, resource groups, metric namespaces, workspaces, and so on —
by forwarding a caller-supplied path to whichever Azure endpoint the
datasource is configured with, using that datasource's own credentials.

That's a broad trust boundary: the proxy's authorization is "can this user
query the datasource at all" (a single, coarse-grained permission), not
"is this specific call one the datasource is designed to make." Anything
the proxy is willing to forward runs with the datasource's own Azure
permissions, which are often broader than any individual read-only call
needs. `resource_allowlist.go` narrows that boundary: it enumerates the
exact `(method, path shape)` pairs the frontend actually issues, and
rejects everything else before it reaches the configured backend.

## Anchoring on the suffix, not the prefix

A call always has two parts:

```
/subscriptions/{sub}/resourceGroups/{rg}/providers/{ns}/{name}/providers/microsoft.insights/metricNamespaces
\____________________ resource identity ____________________/ \_______________ operation _______________/
```

The **resource identity** — which subscription, resource group, or named
resource — is chosen by the user (via the query editor or a template
variable) and is legitimately arbitrary; there's no fixed set of allowed
values to check it against. The **operation** — what's being asked about
that resource — is not arbitrary: it's always one of a small, fixed set of
calls the datasource actually needs to make.

So every pattern in `allowedResourcePaths` is shaped as a free-form middle
section bounded by a **fixed, anchored suffix** (`$`-terminated), not a
prefix match on the identity. That ordering matters: matching only a
leading prefix and trusting whatever follows would let an unrelated
operation be appended after a legitimate-looking identity. Anchoring the
operation at the end of the pattern means the whole string must terminate
there — nothing can be appended after it and still match.

## Why dot-segments and "%" are rejected outright

`isAllowedResourcePath` receives `req.URL.Path`, which `net/http` has
already percent-decoded once. Two consequences of that:

- `net/http.ServeMux`'s own dot-segment cleanup only triggers on a literal
  `..` in the *raw* request target. A percent-encoded one (`%2e%2e`)
  decodes to a literal `..` in `req.URL.Path` without tripping that
  cleanup — so a dot-segment can reach this function looking exactly like
  one that was never caught. None of this datasource's calls ever need a
  dot-segment, so any path containing one is rejected outright rather than
  relying on the shape patterns to happen to reject it.
- A literal `%` surviving that single decode can only be an escaped escape
  (e.g. `%2525` → `%25`) — which risks being decoded a *second* time by
  whatever is on the other end of the proxied request. It's rejected for
  the same reason: the check shouldn't depend on how many times something
  downstream might unescape it.

## Why empty path segments are rejected

The resource-identity portion of a path is built from query-editor
selections and template variables, so an empty or otherwise-degenerate
variable can produce an empty segment (`/subscriptions//...`). The
patterns that need to span a variable number of segments use a wildcard
permissive enough to include `/`, which would otherwise accept an empty
segment exactly as readily as a well-formed one. It's rejected explicitly
so "what happens with an empty or malformed segment" has one answer, not
one that depends on how permissive each individual pattern's wildcard
happens to be.
