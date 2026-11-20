# ojs-mcp: check manifests from an AI agent

`ojs-mcp` is an [MCP](https://modelcontextprotocol.io) server. It lets an AI
agent check the YAML it writes against the schemas on
[openshiftjsonschema.dev](https://openshiftjsonschema.dev), before the YAML
reaches a cluster.

It runs in two ways:

- **Locally.** The agent starts it on your own machine and talks to it over
  stdin/stdout. Your manifests never leave that machine. The only thing it
  does on the network is download the schemas it needs from the site.
- **Hosted.** One copy serves many agents over HTTP. Each caller needs an
  API key and has their own rate limit. See [Host it](#host-it).

## Tools

| Tool | What it does |
| --- | --- |
| `validate` | Checks one or more YAML documents against an OpenShift version. |
| `list_versions` | Lists the OpenShift versions that have schemas. |

`validate` takes:

- `yaml`: the manifest text. Several documents separated by `---` are fine, and
  so is a `kind: List`.
- `version` (optional): `4.22` or `4.22.14`. The newest version is the default.
- `strict` (optional, default `true`): reject fields the schema doesn't know.
  This is what catches a misspelt or misplaced field.

It returns one result per manifest:

```json
{
  "openshift": "4.22",
  "schemas": "v4.22-next-standalone-strict",
  "summary": {"valid": 0, "invalid": 1, "skipped": 0, "errors": 0},
  "results": [{
    "document": 1,
    "kind": "Route",
    "apiVersion": "route.openshift.io/v1",
    "name": "frontend",
    "status": "invalid",
    "problems": [{"path": "/spec", "message": "additional properties 'hostname' not allowed"}]
  }]
}
```

A status is one of:

- `valid` or `invalid`.
- `skipped`: there is no schema for that kind, so it wasn't checked. This is
  normal for custom resources from operators that a base install doesn't
  have, such as cert-manager's `Certificate`.
- `error`: the document isn't valid YAML or isn't a Kubernetes object.

A kind with no schema is `invalid`, not `skipped`, when it is clearly a
mistake rather than a custom resource:

| Manifest has | Problem reported |
| --- | --- |
| `kind: Rout` in `route.openshift.io/v1` | no such kind. Did you mean Route? |
| `HorizontalPodAutoscaler` in `autoscaling/v2beta2` | not in that version; autoscaling has it in version v2 |
| `kind: Route` in `apiVersion: v1` | no such kind; a kind with that name exists in another API group |

To tell the two apart it reads each schema directory's `all.json`, which
lists every type that version has.

The site publishes each version's schemas in several sets. `ojs-mcp` uses the
`next-standalone` set where a version has it (4.18 on) and the `standalone`
set before that. The "next" schemas check manifests more like the API server
does: for example, a required field set to `null` fails, as leaving it out
does. With `strict` on, it uses the `-strict` variant of the same set. The
[site](https://openshiftjsonschema.dev) lists which sets each version has.

It validates with the same library and settings as kubeconform, so the two
should agree.

It only checks structure and types. It never contacts a cluster, so it can't
tell whether a name, an image or a referenced object exists.

## Install

Install it (needs Go 1.26 or later):

```sh
go install github.com/cosmosdevops/ojs-mcp@latest
```

Then tell your agent to run `ojs-mcp`. For Claude Code:

```sh
claude mcp add openshift-json-schema -- ojs-mcp
```

For agents configured with JSON:

```json
{
  "mcpServers": {
    "openshift-json-schema": {"command": "ojs-mcp"}
  }
}
```

To work offline, point it at a copy of the schemas. It needs `versions.json`
and the `v*-standalone*` directories, laid out as on the site:

```sh
ojs-mcp -schemas /path/to/schemas
```

## Host it

With `-http`, `ojs-mcp` serves MCP at `/mcp`. It only answers callers who
send an API key, and each key has its own rate limit.

Keep in mind what hosting means: callers' manifests are sent to your server.
`ojs-mcp` holds them in memory for the length of one call and never writes or
logs them, but they do pass through it and any proxy in front of it.
Manifests can contain secrets, so tell your users, and prefer the local mode
for anything sensitive.

### API keys

Make a key for each person or team:

```sh
sudo sh -c 'ojs-mcp -new-key alice >> /etc/ojs-mcp/keys'
```

This prints the key once, for you to pass on, and adds a line to the keys
file. The file holds a name and the SHA-256 of each key, never the key
itself, so a lost key can't be recovered: make a new one.

```
# /etc/ojs-mcp/keys
alice 5f2b6c…
build-bot 9d41e0…
```

To revoke a key, delete its line. The server notices changes to the file
within a few seconds, with no restart. If an edit leaves the file broken, it
keeps using the keys it had and logs why.

### Rate limits

Each key may make 60 requests a minute, and up to 30 at once before that rate
applies. Change these with `-rate` and `-burst`. Every MCP message counts as
a request, including the few an agent sends when it connects. A caller over
the limit gets `429 Too Many Requests` with a `Retry-After` header.

One key's use doesn't affect another's. Requests without a valid key get
`401` and don't count against anyone.

### Set it up

On a server that has the site's files, with the files in `deploy/`:

```sh
CGO_ENABLED=0 go build -trimpath -o ojs-mcp .       # on any machine
sudo install -m 755 ojs-mcp /usr/local/bin/
sudo useradd --system --no-create-home --shell /usr/sbin/nologin ojs-mcp
sudo install -d -m 750 -o root -g ojs-mcp /etc/ojs-mcp
sudo install -m 640 -o root -g ojs-mcp /dev/null /etc/ojs-mcp/keys
sudo install -m 644 deploy/ojs-mcp.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now ojs-mcp
curl http://127.0.0.1:8087/healthz                  # prints ok
```

Then route `/mcp` to it from Apache. Copy `deploy/ojs-mcp-apache.conf` to
`/etc/apache2/` and include it in the site's HTTPS `<VirtualHost>`:

```sh
sudo a2enmod proxy proxy_http
sudo apachectl configtest && sudo systemctl reload apache2
```

Serve it over HTTPS only: the key travels in a header on every request.

### Or set it up with Docker

There is an image at `ghcr.io/cosmosdevops/ojs-mcp`, built from every commit
on `main`. It holds the one program and nothing else, and runs as an
unprivileged user (ID 65532).

Make the keys directory, readable by that user, then start it with the
compose file from `deploy/`:

```sh
sudo install -d -m 750 -o root -g 65532 /etc/ojs-mcp
sudo install -m 640 -o root -g 65532 /dev/null /etc/ojs-mcp/keys
docker compose -f deploy/compose.yaml up -d
curl http://127.0.0.1:8087/healthz                  # prints ok
```

The compose file mounts the site's document root and the keys directory
read-only, and publishes the port on localhost only. Route `/mcp` to it from
Apache as above.

Make keys with the image too. The key is printed for you; the line for the
file goes to standard output:

```sh
sudo sh -c 'docker run --rm ghcr.io/cosmosdevops/ojs-mcp -new-key alice >> /etc/ojs-mcp/keys'
```

To update, pull and start again. Nothing is lost: there are no sessions.

```sh
docker compose -f deploy/compose.yaml pull && docker compose -f deploy/compose.yaml up -d
```

To build the image yourself instead:
`docker build --build-arg VERSION=1.2.3 -t ojs-mcp .`

**Podman** works too. The image and the compose file run unchanged with
`podman` and `podman-compose`. Two things differ:

- Run as root (`sudo podman …`), the keys directory is set up exactly as
  above.
- Run rootless, user IDs inside the container are shifted, so ID 65532 can't
  read a file that only your own user can. Map the container's user onto
  yours with `--userns=keep-id:uid=65532,gid=65532` (in compose:
  `userns_mode: "keep-id:uid=65532,gid=65532"`), and have your own user own
  the keys directory.

On a host with SELinux (Fedora, RHEL), add `,z` to the two volume lines, as
in `/etc/ojs-mcp:/keys:ro,z`.

The image also works for the local mode, with no Go needed:

```json
{
  "mcpServers": {
    "openshift-json-schema": {
      "command": "docker",
      "args": ["run", "--rm", "-i", "ghcr.io/cosmosdevops/ojs-mcp"]
    }
  }
}
```

### Connect an agent

For Claude Code:

```sh
claude mcp add --transport http openshift-json-schema https://example.com/mcp \
  --header "Authorization: Bearer <key>"
```

For agents configured with JSON:

```json
{
  "mcpServers": {
    "openshift-json-schema": {
      "type": "http",
      "url": "https://example.com/mcp",
      "headers": {"Authorization": "Bearer <key>"}
    }
  }
}
```

### Good to know

- Every call stands alone (no sessions), so restarting loses nothing. Rate
  limit counts do start again after a restart.
- It logs one line per request: the key's name, the status and the time
  taken. Never the request body.
- A call can carry up to 1MB of YAML and 200 documents (`-max-input` changes
  the first).
- It re-reads `versions.json` every hour, so a newly published minor shows
  up without a restart.
- `/healthz` needs no key.

## Develop

```sh
go test ./...
go build        # then point an MCP client at ./ojs-mcp
```

The tests use a tiny fake schema tree in `testdata/site`, so they
don't need the network.

### Releasing

The version is the git tag. Nothing in the code needs changing:

```sh
git tag -a v1.2.3 -m "ojs-mcp 1.2.3"
git push origin v1.2.3
```

CI then publishes the `1.2.3` image, and `go install …@v1.2.3` works.
`ojs-mcp -version` reports the tag in both. A build between tags reports a
version made from the last tag and the commit, and a build with no git
history to read reports `dev`.

## Licence

Apache 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

Not affiliated with or endorsed by Red Hat.
