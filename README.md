# ojs-mcp: check manifests from an AI agent

`ojs-mcp` is an [MCP](https://modelcontextprotocol.io) server. It lets an AI
agent check the YAML it writes against the schemas on
[openshiftjsonschema.dev](https://openshiftjsonschema.dev), before the YAML
reaches a cluster.

The agent starts it on your own machine and talks to it over stdin/stdout.
Your manifests never leave that machine. The only thing it does on the
network is download the schemas it needs from the site.

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

It uses the `next-standalone` schemas where a version has them (4.18 on) and
the `standalone` ones before that. See
[Flavours](https://github.com/cosmosdevops/openshift-json-schema/blob/main/automation/README.md#flavours). It validates with the same
library and settings as kubeconform, so the two should agree.

It only checks structure and types. It never contacts a cluster, so it can't
tell whether a name, an image or a referenced object exists.

## Install

Install it (needs Go 1.25 or later):

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

## Develop

```sh
go test ./...
go build        # then point an MCP client at ./ojs-mcp
```

The tests use a tiny fake schema tree in `testdata/site`, so they
don't need the network.
