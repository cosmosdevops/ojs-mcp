# ojs-mcp

**Validate OpenShift manifests before they reach your cluster.**

`ojs-mcp` is a [Model Context Protocol (MCP)](https://modelcontextprotocol.io) server that lets AI agents validate Kubernetes and OpenShift manifests against the JSON schemas published by [openshiftjsonschema.dev](https://openshiftjsonschema.dev).

When an agent generates or modifies YAML, it can use `ojs-mcp` to check that the resources, fields and API versions are valid before attempting to apply them to a cluster.

No cluster connection is required.

## How it works

There are two ways to run `ojs-mcp`:

- **Local (stdio):** Your AI agent starts `ojs-mcp` as a local process and communicates with it over stdin/stdout. Your manifests stay on your machine. The only network access needed is to download schemas from openshiftjsonschema.dev.
- **Hosted (HTTP):** Run a shared instance that multiple agents can connect to over HTTP. Each client authenticates with an API key and has its own rate limit.

For most individual users, running locally is the simplest option. If you're supporting multiple developers or agents, the hosted option provides a central service.

## Available tools

`ojs-mcp` exposes two MCP tools:

| Tool | Description |
| --- | --- |
| `validate` | Validate one or more YAML manifests against a specific OpenShift version. |
| `list_versions` | List the OpenShift versions with available schemas. |

### Validating manifests

The `validate` tool accepts the following parameters:

| Parameter | Required | Description |
| --- | --- | --- |
| `yaml` | Yes | The YAML to validate. Supports multiple documents separated by `---` and Kubernetes `kind: List` resources. |
| `version` | No | Target OpenShift version, such as `4.22` or `4.22.14`. Defaults to the latest available version. |
| `strict` | No | Whether to reject fields not defined in the schema. Defaults to `true`. |

For example, suppose an agent generates an OpenShift Route with an incorrect field called `hostname`.

The validation result would look something like this:

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

Each manifest receives its own result, so an agent can identify exactly which resource failed and why.

### Understanding validation results

A manifest can have one of four statuses:

| Status | Meaning |
| --- | --- |
| `valid` | The manifest passed schema validation. |
| `invalid` | The manifest contains fields, types or API references that don't match the schema. |
| `skipped` | No schema was available for the resource, so validation wasn't possible. |
| `error` | The input isn't valid YAML or doesn't represent a Kubernetes object. |

**A skipped manifest isn't necessarily a problem.**

For example, a cert-manager `Certificate` is a custom resource that doesn't exist in a standard OpenShift installation. Unless its schema is available, `ojs-mcp` will skip it rather than assume it's invalid.

However, missing schemas can also indicate mistakes, particularly when an agent invents a resource kind or uses the wrong API version.

`ojs-mcp` attempts to distinguish between the two.

For example:

| Manifest | Validation result |
| --- | --- |
| `kind: Rout` with `apiVersion: route.openshift.io/v1` | Invalid. Suggests `Route` as the intended kind. |
| `HorizontalPodAutoscaler` with `apiVersion: autoscaling/v2beta2` | Invalid. The resource is available under `autoscaling/v2`. |
| `kind: Route` with `apiVersion: v1` | Invalid. The resource exists, but belongs to another API group. |
| cert-manager `Certificate` | Skipped. The resource is likely provided by a separately installed operator. |

To make these distinctions, `ojs-mcp` reads the `all.json` index from each schema directory. This provides a list of available resource types and API versions, allowing it to identify likely mistakes rather than simply skipping anything it doesn't recognise.

## Schema validation

`ojs-mcp` uses the schemas published by [openshiftjsonschema.dev](https://openshiftjsonschema.dev).

The site provides several schema sets for each OpenShift version. `ojs-mcp` selects the most appropriate set automatically:

- **OpenShift 4.18 and later:** Uses `next-standalone`.
- **Earlier versions:** Uses `standalone`.
- **Strict validation:** Uses the corresponding `-strict` variant.

The newer `next-standalone` schemas provide validation behaviour closer to the Kubernetes API server. For example, setting a required field to `null` is treated as invalid, just as omitting that field would be.

Validation uses the same library and settings as [kubeconform](https://github.com/yannh/kubeconform), so results should be consistent between the two tools.

### What validation doesn't check

`ojs-mcp` validates the structure and types of your manifests, not whether they can actually be deployed.

It doesn't connect to a Kubernetes or OpenShift cluster, so it can't check things like:

- Whether a referenced Secret, ConfigMap or Service exists.
- Whether a container image is available.
- Whether a resource name is already in use.
- Whether an admission policy would reject the resource.

Think of it as an early validation step, rather than a replacement for server-side validation or admission control.

## Installation

### Install with Go

You'll need Go 1.26 or later.

```sh
go install github.com/cosmosdevops/ojs-mcp@latest
```

Once installed, configure your AI agent to start `ojs-mcp` using stdio.

For **Claude Code**:

```sh
claude mcp add openshift-json-schema -- ojs-mcp
```

For agents that use a JSON configuration:

```json
{
  "mcpServers": {
    "openshift-json-schema": {"command": "ojs-mcp"}
  }
}
```

That's all you need for local use.

Your agent can now discover the `validate` and `list_versions` tools and use them when working with OpenShift manifests.

### Using local schemas

By default, `ojs-mcp` downloads the schemas it needs from openshiftjsonschema.dev.

If you want to run completely offline, or already have a local copy of the schemas, you can point it at that directory:

```sh
ojs-mcp -schemas /path/to/schemas
```

The directory should contain `versions.json` and the `v*-standalone*` schema directories, following the same structure as the website.

## Hosting an MCP server

If you're using `ojs-mcp` across a team or want to make validation available to several agents, you can run it as a shared HTTP service.

Start it with `-http` to expose an MCP endpoint at `/mcp`.

The hosted service includes:

- API key authentication.
- Per-key rate limiting.
- Automatic key-file reloading.
- A health-check endpoint.
- Request logging without recording manifest contents.

**A note on security:** Unlike local mode, hosted mode sends manifests over the network to your server.

`ojs-mcp` processes manifests in memory and doesn't write or log their contents. However, the manifests still travel through your network and any reverse proxies in front of the service.

Since Kubernetes manifests can contain sensitive information, including Secrets, make sure users understand this distinction. Local mode is generally preferable when handling sensitive workloads.

### API keys

Each user, team or automated client should have its own API key.

Generate one with:

```sh
sudo sh -c 'ojs-mcp -new-key alice >> /etc/ojs-mcp/keys'
```

The command generates a key and adds its entry to the keys file.

The actual API key is displayed only once. The keys file stores a name and a SHA-256 hash, not the original key.

For example:

```text
# /etc/ojs-mcp/keys
alice 5f2b6c…
build-bot 9d41e0…
```

If someone loses their key, generate a replacement. There's no way to recover the original from the stored hash.

To revoke access, simply remove the corresponding line from the file.

The server checks the file for changes and picks them up within a few seconds, without requiring a restart.

If the updated file is malformed, the server logs the problem and continues using the last valid configuration.

### Rate limiting

Each API key has its own rate limit, so one client can't consume another client's allowance.

The defaults are:

| Setting | Default | Flag |
| --- | --- | --- |
| Requests per minute | 60 | `-rate` |
| Burst allowance | 30 | `-burst` |

These can be adjusted when starting the server.

Rate limiting applies to every MCP message, including the initial messages exchanged when an agent connects.

If a client exceeds its allowance, the server responds with:

```http
429 Too Many Requests
Retry-After: <seconds>
```

Requests without a valid API key receive `401 Unauthorized` and don't count against any client's limit.

## Deploying the server

There are a few ways to host `ojs-mcp`, depending on your environment.

### Option 1: Native systemd service

If you're hosting the service on a Linux server that already has the openshiftjsonschema.dev files available, you can run it directly under systemd.

Build the binary:

```sh
CGO_ENABLED=0 go build -trimpath -o ojs-mcp .
```

Install it and create a dedicated service account:

```sh
sudo install -m 755 ojs-mcp /usr/local/bin/
sudo useradd --system --no-create-home --shell /usr/sbin/nologin ojs-mcp
```

Create the configuration directory and keys file:

```sh
sudo install -d -m 750 -o root -g ojs-mcp /etc/ojs-mcp
sudo install -m 640 -o root -g ojs-mcp /dev/null /etc/ojs-mcp/keys
```

Install the provided systemd unit:

```sh
sudo install -m 644 deploy/ojs-mcp.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now ojs-mcp
```

Check that the service is running:

```sh
curl http://127.0.0.1:8087/healthz
```

A healthy instance returns:

```text
ok
```

#### Configure Apache

The repository includes an example Apache configuration in `deploy/ojs-mcp-apache.conf`.

Copy it to `/etc/apache2/` and include it in your site's HTTPS `<VirtualHost>`.

Enable the required modules and reload Apache:

```sh
sudo a2enmod proxy proxy_http
sudo apachectl configtest
sudo systemctl reload apache2
```

Your reverse proxy should forward `/mcp` to the local service.

**Always expose the hosted endpoint over HTTPS.** API keys are sent in an HTTP header with every request.

### Option 2: Docker

A container image is available at:

```text
ghcr.io/cosmosdevops/ojs-mcp
```

A new image is built from every commit to `main`.

The image contains the `ojs-mcp` binary and runs as an unprivileged user (UID 65532).

The repository also includes a ready-to-use Compose file under `deploy/`.

First, create the keys directory and ensure the container user can read it:

```sh
sudo install -d -m 750 -o root -g 65532 /etc/ojs-mcp
sudo install -m 640 -o root -g 65532 /dev/null /etc/ojs-mcp/keys
```

Start the service:

```sh
docker compose -f deploy/compose.yaml up -d
```

Verify that it's running:

```sh
curl http://127.0.0.1:8087/healthz
```

The Compose configuration mounts the schema files and API key directory as read-only volumes. It also binds the service to localhost, so you'll need a reverse proxy to make it externally accessible.

You can use the Apache configuration described above.

#### Generate API keys with Docker

You don't need to install the binary on the host to generate API keys.

Use the container instead:

```sh
sudo sh -c 'docker run --rm ghcr.io/cosmosdevops/ojs-mcp -new-key alice >> /etc/ojs-mcp/keys'
```

As with the native installation, the key is displayed once and its hash is stored in the keys file.

#### Updating

Pull the latest image and recreate the service:

```sh
docker compose -f deploy/compose.yaml pull
docker compose -f deploy/compose.yaml up -d
```

`ojs-mcp` doesn't maintain persistent MCP sessions, so restarting the service doesn't lose any session state.

If you'd rather build the image yourself:

```sh
docker build --build-arg VERSION=1.2.3 -t ojs-mcp .
```

### Option 3: Podman

The same container image works with Podman, and you can use the provided Compose file with `podman-compose`.

There are a couple of things to consider.

**Rootful Podman**

If you're running Podman as root (`sudo podman`), use the same keys directory permissions as the Docker setup.

**Rootless Podman**

Rootless Podman uses user namespaces, which means the container's UID 65532 may not map directly to your host user.

You can map the container user to your own account using:

```sh
--userns=keep-id:uid=65532,gid=65532
```

Or set this in your Compose configuration:

```yaml
userns_mode: "keep-id:uid=65532,gid=65532"
```

Make sure your host user owns the keys directory.

**SELinux**

On Fedora, RHEL and other SELinux-enabled systems, add `,z` to the volume mounts so the container can access them with the appropriate SELinux labels.

For example:

```text
/etc/ojs-mcp:/keys:ro,z
```

### Running locally with a container

You can also use the container image as a local stdio MCP server, without installing Go or the binary.

For agents using JSON configuration:

```json
{
  "mcpServers": {
    "openshift-json-schema": {
      "command": "docker",
      "args": [
        "run",
        "--rm",
        "-i",
        "ghcr.io/cosmosdevops/ojs-mcp"
      ]
    }
  }
}
```

This runs a new container when your agent starts the MCP server and removes it when the process exits.

It behaves the same way as the native local installation.

## Connecting to a hosted instance

Once your HTTP service is running behind HTTPS, you can connect your agents to it.

### Claude Code

```sh
claude mcp add --transport http openshift-json-schema https://example.com/mcp \
  --header "Authorization: Bearer <key>"
```

Replace the example URL and API key with your own.

### JSON-based configuration

For other MCP clients that support HTTP transport:

```json
{
  "mcpServers": {
    "openshift-json-schema": {
      "type": "http",
      "url": "https://example.com/mcp",
      "headers": {
        "Authorization": "Bearer <key>"
      }
    }
  }
}
```

Each client should use its own API key, particularly when sharing an instance across teams.

## Operational details

A few things worth knowing when running a hosted instance:

- **Stateless requests:** Every call is independent. There are no persistent MCP sessions to manage, and restarting the server doesn't interrupt stored session state.
- **Rate limits reset on restart:** Rate limit counters are held in memory and start fresh when the process restarts.
- **Request logging:** The server logs the API key name, response status and request duration. It never logs request bodies or manifest contents.
- **Input limits:** A validation request can contain up to 1 MB of YAML and 200 documents. The input size limit can be changed with `-max-input`.
- **Schema updates:** The server reloads `versions.json` every hour, so newly published OpenShift minor versions become available without restarting.
- **Health checks:** `/healthz` is available without authentication, making it suitable for monitoring and container health checks.

## Development

To work on `ojs-mcp` locally, clone the repository and run the tests:

```sh
go test ./...
```

Build the binary:

```sh
go build
```

You can then configure your MCP client to use the locally built `./ojs-mcp` binary.

The tests use a small set of example schemas under `testdata/site`, so you don't need network access or a full copy of the OpenShift schemas to run them.

### Releasing a new version

Releases are based on Git tags. There's no need to update a version constant in the source code.

Create and push a tag:

```sh
git tag -a v1.2.3 -m "ojs-mcp 1.2.3"
git push origin v1.2.3
```

CI will publish the corresponding `1.2.3` container image.

The tagged version can also be installed directly with Go:

```sh
go install github.com/cosmosdevops/ojs-mcp@v1.2.3
```

To check the version of a running binary:

```sh
ojs-mcp -version
```

For tagged builds, this reports the release version. Builds made between tags include information from the latest tag and commit. If Git history isn't available, the version is reported as `dev`.

## Licence

Licensed under the Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE) for details.

This project is not affiliated with or endorsed by Red Hat.
