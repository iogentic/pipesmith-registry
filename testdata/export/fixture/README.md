# Fixture Export

The identity set of the fixture database of `tests/import/`, written by the export program with password hashes:

```
tools/h2export.sh fixture .tmp/import/x/fixture.mv.db
tools/h2export.sh export --database .tmp/import/x/fixture.mv.db --out testdata/export/fixture --set identity --with-password-hashes
```

`../fixture-content/` is the content set of the same database (`--set content`, blob stores and repositories). `tests/import/test_h2export.py` checks that both folders equal a fresh export byte for byte. Do not edit the files by hand: change `tests/import/FixtureDb.java` and export again.

Every name, address and password here is invented. The hashes are Shiro 1 hashes (SHA-512, 1024 rounds) of these passwords:

| User | Password | Status | Roles |
| --- | --- | --- | --- |
| `admin` | `example-admin-1` | active | `nx-admin` (privilege `nx-all`) |
| `viewer` | `example-viewer-1` | active | `demo-viewer` (`nx-repository-view-*-*-read`, `nx-repository-view-*-*-browse`, `nx-search-read`) |
| `maven-lead` | `example-maven-1` | active | `demo-maven-lead` (stored privileges `demo-maven-write`, `demo-csel-maven`, and the role `demo-viewer`) |
| `locked.user` | `example-locked-1` | locked | `demo-viewer` |

Also in the set: the role `developers`, named like a directory group; the role `demo-ops`, which contains a role that does not exist; one stored privilege of each of the six types; the directory user `ldap.person` with an explicit mapping of source `LDAP`; the LDAP connection `demo-directory` to `ldap.example.com`, its bind password removed by the export; the realms `NexusAuthenticatingRealm`, `NexusAuthorizingRealm`, `LdapRealm`, `DockerToken`; anonymous access off.

The demo lays this set over a real repository list: `pipesmith-registry import testdata/export/fixture` replaces users, roles, privileges, LDAP connections, realms and the anonymous setting, and leaves repositories and blob stores as they are.
