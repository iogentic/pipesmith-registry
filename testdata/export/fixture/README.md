# Fixture Export

The identity set of the fixture database of `tests/import/`, written by the export program with password hashes:

```
tools/h2export.sh fixture .tmp/import/x/fixture.mv.db
tools/h2export.sh export --database .tmp/import/x/fixture.mv.db --out .tmp/import/x/identity --set identity --with-password-hashes
cp .tmp/import/x/identity/* testdata/export/fixture/
```

`../fixture-content/` is the content set of the same database (`--set content`: blob stores, repositories, routing rules, cleanup policies, capabilities and the tasks of the scheduler). Its stores without a soft quota carry an empty `blobStoreQuotaConfig`. The Docker port of `demo-docker` is written `14511.0`. The old product stores both values this way. The three invented routing rules are `demo-block-routed` (BLOCK, the only rule a repository names: `demo-maven-proxy`; its matchers touch no path another suite uses), `demo-allow-releases` (ALLOW) and `demo-java-only` (BLOCK, a look ahead that Java compiles and Go does not). `tests/import/test_h2export.py` checks that both folders equal a fresh export byte for byte. Do not edit the files by hand: change `tests/import/FixtureDb.java` and export again. The export refuses a folder that is not empty, so it goes to an empty folder first.

Every name, address and password here is invented. The hashes are Shiro 1 hashes (SHA-512, 1024 rounds) of these passwords:

| User | Password | Status | Roles |
| --- | --- | --- | --- |
| `admin` | `example-admin-1` | active | `nx-admin` (privilege `nx-all`) |
| `viewer` | `example-viewer-1` | active | `demo-viewer` (`nx-repository-view-*-*-read`, `nx-repository-view-*-*-browse`, `nx-search-read`) |
| `maven-lead` | `example-maven-1` | active | `demo-maven-lead` (stored privileges `demo-maven-write`, `demo-csel-maven`, and the role `demo-viewer`) |
| `locked.user` | `example-locked-1` | locked | `demo-viewer` |

Also in the set: the role `developers`, named like a directory group; the role `demo-ops`, which contains a role that does not exist; one stored privilege of each of the six types; the directory user `ldap.person` with an explicit mapping of source `LDAP`; the LDAP connection `demo-directory` to `ldap.example.com` in the shape the old product stores it (the host as an object, `systemUsername`, no group type), its bind password `systemPassword` removed by the export; the realms `NexusAuthenticatingRealm`, `NexusAuthorizingRealm`, `LdapRealm`, `DockerToken`; anonymous access off.

The demo lays this set over a real repository list: `pipesmith-registry import testdata/export/fixture` replaces users, roles, privileges, LDAP connections, realms and the anonymous setting, and leaves repositories and blob stores as they are.

## Administration Rows

The content set also holds these invented rows (`tests/import/FixtureDb.java`):

| File | Rows | Removed | Rows by name |
| --- | ---: | ---: | --- |
| `cleanup_policy.jsonl` | 3 | 0 | `demo-age`, `demo-mixed` (holds the criterion `regex`, which this server does not know), `demo-unused` |
| `capability_storage_item.jsonl` | 3 | 1 | `baseurl`, `demo.webhook` (its property `secret` removed), `defaultrole` |
| `qrtz_job_details.jsonl` | 6 | 2 | `demo-cron`, `demo-manual`, `demo-monthly` (the last day of the month, 999), `demo-stricter` (a cron expression this server refuses), `demo-weekly` (stored as the scheduler's map classes, paused, its property `remotePassword` removed), `demo-unreadable` (its job data is a list, written as `null`) |
| `qrtz_triggers.jsonl` | 6 | 0 | one trigger per job, `<job>-trigger`, with the schedule in its job data |
