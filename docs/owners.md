# Owners: users, the instance organization and mirror owners

Every module lives under an owner: `<host>/<owner>/<module>`. Protodex uses a
**single-organization model**: an instance normally has one organization
(the company or team running it) plus its users. The owners table still holds
any number of owners, so extra organizations (teams, namespaces) and
**mirror owners** can coexist.

- **Users** authenticate with tokens and can own modules. Names: 1-32
  characters.
- **Organizations** own shared modules. Names: 1-48 characters.
- **Mirror owners** are organizations that reserve an upstream owner name such
  as `googleapis` or `bufbuild`, so mirrored modules keep their upstream path
  (see [ADR 0001](adr/0001-naming-and-addressing.md)). A mirror owner keeps its
  name: it cannot be renamed.

All names use lowercase letters, digits and hyphens, start with a letter and
do not end with a hyphen. Users, organizations and mirror owners share one
namespace: a name can be used by only one owner, so a local owner can never
shadow a mirror.

Owner IDs never change. Renaming a user or organization keeps its modules and
tokens; the old name becomes free (there is no redirect).

## Bootstrap

```sh
protodex bootstrap -db protodex.db -user admin -org acme -mirror-owners googleapis,bufbuild
```

Creates the first user (printing its token once), the instance organization
and the mirror owners. Rerunning is safe: existing owners of the same kind
are left alone, and a name held by another kind of owner is an error.

| Flag             | Environment               | Meaning                                |
|------------------|---------------------------|----------------------------------------|
| `-db`            | `PROTODEX_DB_PATH`        | SQLite database                        |
| `-user`          | `PROTODEX_BOOTSTRAP_USER` | first user (default `admin`)           |
| `-org`           | `PROTODEX_BOOTSTRAP_ORG`  | instance organization (optional)       |
| `-mirror-owners` | `PROTODEX_MIRROR_OWNERS`  | comma-separated reserved mirror owners |
| –                | `PROTODEX_BOOTSTRAP_TOKEN`| use this token instead of generating one |

## Managing owners

```sh
protodex owner create -db protodex.db user alice bob   # prints a token per user
protodex owner create -db protodex.db org platform
protodex owner create -db protodex.db mirror grpc-ecosystem
protodex owner list   -db protodex.db
protodex owner rename -db protodex.db platform platform-team
```

`create` is all-or-nothing: if one name is taken, none are created. The
commands take the database's exclusive lock, so run them while the server is
stopped.

## API

With a database configured, the server answers:

| RPC | Behaviour |
|-----|-----------|
| `OwnerService.GetOwners` | Resolves owner names/IDs to users or organizations; any unknown owner is `NotFound`. |
| `UserService.GetUsers`, `GetCurrentUser`, `ListUsers` | `GetCurrentUser` needs a token. Users are reported as standard and active. |
| `OrganizationService.GetOrganizations`, `ListOrganizations` | Mirror owners are listed as organizations. |

Reads work anonymously. List RPCs page with `page_size` (max 250) and an
opaque `page_token`. Filtering by organization membership (`ListUsers`
`organization_refs`, `ListOrganizations` `user_refs`) or by instance role is
not modeled yet and returns `Unimplemented`. Creating, updating and deleting
users and organizations over the API, and organization settings, return
`Unimplemented`: use the `protodex owner` command.
