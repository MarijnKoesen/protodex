# Permissions: roles, visibility and contributors

Protodex decides every request in two layers:

1. **Procedure policy** (an interceptor, `internal/server/permissions.go`).
   Every RPC is listed in one table as *anonymous*, *authenticated*,
   *write* or *admin*. Procedures that are not listed are denied, and a test
   checks that the table covers every vendored registry RPC.
2. **Resource checks** in the handlers: the caller's permission on each
   module (or owner, for creation) the request touches.

## Instance roles

Every user has one instance role:

| Role | Meaning |
|------|---------|
| `viewer` (read-only) | Reads public modules, organization modules and their own namespace. Cannot create, push, label, change or delete anything, unless a module grants them more (see contributors). |
| `member` | Full access to their own namespace and to organization namespaces (single-organization model). Cannot read or change other users' private modules. The default for new users. |
| `admin` | Full access to every module of every owner, except mirror modules. Required for the admin-only RPCs (organization and user management, see [owners.md](owners.md)). |

Roles come from:

- `protodex bootstrap`: the first user is an `admin`.
- `protodex owner create [-role viewer|member|admin] user NAME...` (default
  `member`) and `protodex owner role USER ROLE`.
- OIDC login: the role derived from the IdP's groups (`group_roles`,
  `default_role`; see the [README](../README.md#oidc-login)) replaces the
  user's role on **every** login, so for OIDC users the IdP is the source of
  truth.

`protodex owner list` shows each user's role. Over the API, `UserService`
reports admins as `USER_INSTANCE_ROLE_ADMIN` and members and viewers as
`USER_INSTANCE_ROLE_MEMBER` (the API has no read-only role), and
`ListUsers` can filter on `has_roles`.

**Upgrading:** migration 7 makes every existing user a `member`, which
keeps the access they had before roles were enforced. Promote your
administrators afterwards with `protodex owner role NAME admin`.

## Module visibility

- **Public** modules are readable by anyone, including anonymous callers.
- **Private** modules (the default) are readable only by callers with a
  permission on them (below).
- A module the caller cannot read is reported as `NotFound`, never
  `PermissionDenied`, so its existence does not leak. This also holds for
  dependencies: `GraphService.GetGraph` and
  `FileDescriptorSetService.GetFileDescriptorSet` of a readable module fail
  with `NotFound` when a transitive dependency is not readable, and a push
  may only depend on commits the pusher can read. `DownloadService.Download`
  returns a commit's own files only, so a public module stays downloadable
  even if it depends on a private one.

## Module permissions

A caller's permission on a module is the highest of:

| Source | Permission |
|--------|------------|
| Module is public | read |
| Instance `admin` | admin |
| `member` on their own namespace or an organization's | admin |
| `viewer` on their own namespace or an organization's | read |
| Contributor grant on the module | read, write or admin |

…capped at **read** for modules of **mirror owners**: those are managed by
mirroring and are read-only over the API for everyone, admins included.

What each permission allows:

| Action | Needs |
|--------|-------|
| Get/list the module, its commits, labels, resources, files, graph, descriptors | read |
| Push (`UploadService.Upload`), create/move/archive/unarchive labels | write |
| Change settings (`UpdateModules`: visibility, description, URL, default label, deprecation) and delete | admin |
| Create a module under an owner | `member` on their own namespace or an organization, or instance `admin`; never under a mirror owner |
| Manage own tokens (`TokenService`), `GetCurrentUser` | any authenticated user |

Write procedures are closed to `viewer`s at the interceptor unless they
hold at least one write or admin grant; the handler then checks the grant
for the specific module.

## Contributors

A contributor grant gives one user a role on one module: `read`, `write`
or `admin`. Grants only add access; they never reduce what a user's
instance role already allows. They are removed with the module or the
user. Mirror modules take no grants.

```sh
protodex contributor add    -db protodex.db acme/weather alice write
protodex contributor list   -db protodex.db acme/weather
protodex contributor remove -db protodex.db acme/weather alice
```

Like `protodex owner`, these commands take the database's exclusive lock,
so run them while the server is stopped.

The `buf` CLI has no commands for roles or contributors, so protodex does
not serve an API for them (no `iam/v1` permission or contributor services
are vendored).

## Error codes

| Condition | Code |
|-----------|------|
| No token on a non-anonymous RPC | `Unauthenticated` |
| Module (or dependency) not readable by the caller | `NotFound` |
| Readable module, but missing write/admin permission; any write to a mirror module; creating under another user's or a mirror owner's namespace; a `viewer` calling a write RPC without any write grant; a non-admin calling an admin RPC | `PermissionDenied` |
