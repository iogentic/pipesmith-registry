# H2 Export

`H2Export.java` reads the identity and configuration tables of the old product's embedded H2 database and writes one JSON lines file per table plus `manifest.json`. `tools/h2export.sh` runs it in the Java image pinned there, with the H2 driver 2.2.224, and is the only supported way to start it.

```
tools/h2export.sh selftest
tools/h2export.sh export --database .tmp/import/x/fixture.mv.db --out .tmp/import/x/out --set all
tools/h2export.sh export --database-copy <copy>.mv.db --out work/export --set all --with-password-hashes
```

## Sets

| Set | Tables |
| --- | --- |
| `content` (default) | `BLOB_STORE_CONFIGURATION`, `REPOSITORY` |
| `identity` | `SECURITY_USER`, `ROLE`, `USER_ROLE_MAPPING`, `PRIVILEGE`, `REALM_CONFIGURATION`, `ANONYMOUS_CONFIGURATION`, `LDAP_CONFIGURATION` |
| `all` | both, identity first |

Each table has a fixed list of columns (`TABLES` in `H2Export.java`). No other table and no other column is read, and no statement selects `*`. A missing table or column ends the run with exit 4 and its name.

## Output

- One file `<table>.jsonl` per table, lower case, one JSON object per row with the column names in lower case as keys, rows in a fixed order (the id, or the name where the id is a random UUID). JSON columns are nested values. Numbers keep the text the database holds: `14511.0` stays `14511.0`, and `14511` stays `14511` (fixture, 2026-11-18). The old product stores some integers as floating point values. The import reads a zero fraction as the integer. Every row has `_removed`: the paths of the fields taken out, never their values.
- `manifest.json`: format, version 1, the set, the driver version, whether hashes are in, and per file the table, the columns, the row count, the count of removed fields and the SHA-256 of the file. No clock time, so two exports of one database are equal byte for byte.
- The directory has mode 0700, every file 0600. A failed run removes the files it wrote.

## Secrets

- Inside the Java process, before a byte is written, every key of a JSON column whose name contains `password`, `secret`, `token`, `key`, `passphrase` or `credential` (any case, at any depth) is removed. This takes out the HTTP client password and bearer token of a proxy repository, the bind password of an LDAP connection, and the keys of a cloud blob store. It also takes out `userPasswordAttribute` of the LDAP mapping, an attribute name and no secret, because the rule has no exceptions.
- A JSON column that holds no object or list (for instance an encrypted string) is written as `null` and listed in `_removed`.
- `SECURITY_USER.PASSWORD` is selected only with `--with-password-hashes`. Without it the column is not read and every row carries `""`.
- The secret store and token tables are not in the list and are never read. The fixture of `tests/import/` puts canaries into two of them.
- No value of a row is printed. Errors name the table, the column, the H2 error code and the SQL state.

## Paths

- `--database` must resolve, links followed, below `work/` or `.tmp/` of this repository. Any other database is named with `--database-copy`.
- `--out` must not exist or be empty. It lies below `work/` or `.tmp/`, or below `testdata/export/` for a database that is not a `--database-copy`, so the export of a real database cannot land in a committed folder.
- The database directory is mounted read only, the container has no network, and the program compares size and modification time of the data file before and after.

## Exit Codes

0 done, 1 failed, 2 usage, 3 refused path, 4 schema mismatch, 5 the database cannot be opened or the driver is not 2.2.224.

## Driver

H2 2.2.224 from Maven Central, `https://repo1.maven.org/maven2/com/h2database/h2/2.2.224/h2-2.2.224.jar`, 2,614,933 bytes. SHA-256 `b9d8f19358ada82a4f6eb5b174c6cfe320a375b5a9cb5a4fe456d623e6e55497`, the same as the `.sha256` Maven Central publishes; SHA-1 `7bdade27d8cd197d9b5ce9dc251f41d2edc5f7ad`, the same as the published `.sha1`. Licence MPL 2.0 or EPL 1.0 (its pom). Read on 2026-11-12. The version is the one that wrote the database of the install.

## Sources

Read on 2026-11-12. S1 to S5 are files of the public source repository of the old product at tag `release-3.87.2-01`; the table names the files without their package paths.

| Id | Files | What it gives |
| --- | --- | --- |
| S1 | `CUserDAO.xml`, `CRoleDAO.xml`, `CPrivilegeDAO.xml`, `CUserRoleMappingDAO.xml` of the security model | tables `security_user`, `role`, `privilege`, `user_role_mapping` with their columns |
| S2 | `RealmConfigurationDAO.xml`, `AnonymousConfigurationDAO.xml` | tables `realm_configuration`, `anonymous_configuration` |
| S3 | `ConfigurationDAO.xml` of the repository configuration, `BlobStoreConfigurationDAO.xml` | tables `repository`, `blob_store_configuration` |
| S4 | `AbstractJsonTypeHandler.java` of the MyBatis data store | JSON columns are bound as UTF-8 bytes for H2 and read with `getBytes` |
| S5 | `AuthenticationConfigurationSerializer.java`, `HttpClientFacetImpl.java`, `FileBlobStore.java`, `BlobStoreQuotaSupport.java`, `BlobStoreGroup.java`, `GroupFacetImpl.java` | the keys `httpclient.authentication` with `password` and `bearerToken`, `file.path`, `blobStoreQuotaConfig.quotaType` and `quotaLimitBytes`, `group.members`, `group.memberNames` |
| S6 | a schema list of the old database, table and column names only, read from a copy on 2026-11-11 | the column names and types, among them `LDAP_CONFIGURATION` with `ID`, `NAME`, `order`, `CONNECTION`, `MAPPING` |
| S7 | read on 2026-11-18. At tag `release-2.15.1-02` of the same repository, the 2.x line: `DefaultLdapGroupDAO.java` (`getGroupMembership`), `DefaultLdapConnector.java` (`isStaticGroupMapping`), the model `ldap.xml`. At `release-3.87.2-01`: `LdapServersUserAndGroupMachine.js`, `LdapServers.testdata.js`, `LdapStrings.jsx`, `LdapServerConnectionFieldSet.js`, `PluginStrings.js` of the user interface | groups are dynamic when `userMemberOfAttribute` is not empty after trimming, else static; the stored names `systemUsername`, `emailAddressAttribute`, `ldapFilter`, `userMemberOfAttribute`; the 3.x interface labels the timeout and the retry delay in seconds |
| S8 | read on 2026-11-18 at `release-3.87.2-01`: `BlobStoreApiModel.java` (`createSoftQuota`), `BlobStoreConfigurationData.java` (`attributes`) | a store whose `blobStoreQuotaConfig` is empty has no soft quota; reading the quota of a store creates that empty object, so stores without a quota carry `{}` |

## LDAP

`LDAP_CONFIGURATION.CONNECTION` and `MAPPING` hold the fields of the stored entity of the old product, not the names of its REST surface. The key names and types are known from the first import of a real export on 2026-11-18. The values are not. `host` is an object of `hostName`, `port` and `protocol`. Six stored names stand for names of the REST surface: `systemUsername` for `authUsername`, `saslRealm` for `authRealm`, `connectionTimeout` for `connectionTimeoutSeconds`, `connectionRetryDelay` for `connectionRetryDelaySeconds`, `emailAddressAttribute` for `userEmailAddressAttribute`, `ldapFilter` for `userLdapFilter`. The row has no group type. `userMemberOfAttribute` is null for static groups. The bind password is `systemPassword`. The fixture writes this shape. The import reads it, still reads the REST names of the 0.1.1 fixture, and refuses a row that gives one field in both forms. A row without a group type is dynamic when `userMemberOfAttribute` is set and not blank, else static (S7).

## Unverified

- The mapper of `LDAP_CONFIGURATION` and the code of the 3.x line that reads the stored entity are not in the public source. The group type rule is the one of the 2.x line (S7). The 3.x user interface sends no member of attribute for static groups and no group fields for dynamic ones. This fits the rule. That the stored `connectionTimeout` and `connectionRetryDelay` count seconds rests on the 3.x user interface, which labels both in seconds and sends `connectionTimeout` unchanged as `connectionTimeoutSeconds` (S7).
- Whether the bind password sits inside `CONNECTION` in clear, as a reference into the secret store, or whether the whole column is encrypted. Each of the three forms is covered: a key with `password` in its name goes, and a column that is no object goes.
- The column list of the install is checked by name only (S6). A type that differs from the fixture shows at the first real run.
