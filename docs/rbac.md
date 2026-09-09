# Kubernetes RBAC by operation

Generated from `runtime.KubernetesPermissions`, also used by `debux doctor --mode`.
Do not edit the permission tables by hand.

Profiles select Linux privileges; they do not change API permissions. Bind only the roles needed for your operation, in the namespace you intend to debug. These examples create roles, not role bindings. Both POST (SPDY) and GET (WebSocket fallback) are listed for exec/attach.

`cp` uses `ephemeral` permissions. Listing all namespaces additionally requires a cluster-scoped pod list permission. Optional event-list access improves failure diagnostics; it is not required to start a session.

## ephemeral

```sh
debux doctor --context CONTEXT --namespace prod --mode ephemeral --strict
```

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: debux-ephemeral
  namespace: prod
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["pods/ephemeralcontainers"]
    verbs: ["update"]
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["create", "get"]
```

## copy

```sh
debux doctor --context CONTEXT --namespace prod --mode copy --strict
```

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: debux-copy
  namespace: prod
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch", "create", "delete"]
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["create", "get"]
```

## pod

```sh
debux doctor --context CONTEXT --namespace prod --mode pod --strict
```

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: debux-pod
  namespace: prod
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "watch", "create", "delete"]
  - apiGroups: [""]
    resources: ["pods/attach"]
    verbs: ["create", "get"]
```

## node

```sh
debux doctor --context CONTEXT --namespace prod --mode node --strict
```

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: debux-node
  namespace: prod
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "watch", "create", "delete"]
  - apiGroups: [""]
    resources: ["pods/attach"]
    verbs: ["create", "get"]
```

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: debux-node
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list"]
```

## forward

```sh
debux doctor --context CONTEXT --namespace prod --mode forward --strict
```

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: debux-forward
  namespace: prod
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["pods/portforward"]
    verbs: ["create"]
```
