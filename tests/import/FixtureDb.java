// Builds the fixture database of tests/import: an H2 2.2.224 file with the
// tables of the old product's public schema and invented rows only.
//
// usage: java -cp h2-2.2.224.jar FixtureDb.java <base> [--missing-column TABLE.COLUMN | --secret-store <rows>]
//
// <base> is the path without ".mv.db" and must not exist. User and password
// are empty, as the old product opens its embedded file. JSON columns are
// bound as UTF-8 bytes, as the old product's type handler binds them for H2.
// The passwords below are invented and belong to no one; their hashes are
// Shiro 1 hashes (SHA-512, 1024 rounds) with a salt derived from the user id,
// so every build gives the same rows. The CANARY values stand for secrets:
// none of them may reach an export. --missing-column leaves one column out,
// for the schema check of the export.
//
// --secret-store gives SECRETS and API_KEY_V2 the columns of the old product
// and fills them from <rows>, a file of tab separated lines that a test
// writes at run time (tests/import/test_h2export.py): "secret", id, purpose,
// key id (empty for none), stored value; "apikey", user, realm (or "-"),
// domain, access key, secret reference, created; "replace", a CANARY value
// of a JSON column, the value that takes its place. The principals of a key
// are a Java serialized map of the realm to the user, as the old product
// keeps a realm name inside its serialized principals.

import java.io.ByteArrayOutputStream;
import java.io.ObjectOutputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.Statement;
import java.sql.Types;
import java.util.Arrays;
import java.util.Base64;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;

public final class FixtureDb {
    static final Map<String, String> PASSWORDS = new LinkedHashMap<>();
    static {
        PASSWORDS.put("admin", "example-admin-1");
        PASSWORDS.put("viewer", "example-viewer-1");
        PASSWORDS.put("maven-lead", "example-maven-1");
        PASSWORDS.put("locked.user", "example-locked-1");
    }

    // Column lists after the public mapper files; LDAP_CONFIGURATION after the
    // schema list of the lab notes, as its mapper is not public.
    static final String[] SCHEMA = {
        "CREATE TABLE security_user (id VARCHAR(200) NOT NULL, first_name VARCHAR(200) NULL, last_name VARCHAR(200) NULL,"
            + " password VARCHAR(200) NOT NULL, status VARCHAR(100) NOT NULL, email VARCHAR(200) NOT NULL,"
            + " version INT NOT NULL DEFAULT 1, CONSTRAINT pk_security_user_id PRIMARY KEY (id))",
        "CREATE TABLE role (id VARCHAR(200) NOT NULL, name VARCHAR(200) NOT NULL, description VARCHAR(400) NULL,"
            + " privileges JSON NOT NULL, roles JSON NOT NULL, read_only BOOLEAN NOT NULL, version INT NOT NULL DEFAULT 1,"
            + " CONSTRAINT pk_role_id PRIMARY KEY (id))",
        "CREATE TABLE user_role_mapping (user_id VARCHAR(200) NOT NULL, user_lo VARCHAR(200) NULL,"
            + " source VARCHAR(100) NOT NULL, roles JSON NOT NULL, version INT NOT NULL DEFAULT 1,"
            + " CONSTRAINT pk_user_role_mapping_user_id_source PRIMARY KEY (user_id, source))",
        "CREATE TABLE privilege (id VARCHAR(200) NOT NULL, name VARCHAR(200) NOT NULL, description VARCHAR(400) NULL,"
            + " type VARCHAR(100) NOT NULL, properties JSON NOT NULL, read_only BOOLEAN NOT NULL,"
            + " version INT NOT NULL DEFAULT 1, CONSTRAINT pk_privilege_id PRIMARY KEY (id))",
        "CREATE TABLE realm_configuration (id INT NOT NULL CHECK (id = 1), realm_names JSON NOT NULL,"
            + " CONSTRAINT pk_realm_configuration_id PRIMARY KEY (id))",
        "CREATE TABLE anonymous_configuration (id INT NOT NULL CHECK (id = 1), enabled BOOLEAN NOT NULL,"
            + " user_id VARCHAR(200) NOT NULL, realm_name VARCHAR(200) NOT NULL,"
            + " CONSTRAINT pk_anonymous_configuration_id PRIMARY KEY (id))",
        "CREATE TABLE ldap_configuration (id UUID NOT NULL, name VARCHAR(200) NOT NULL, \"order\" INT NOT NULL,"
            + " connection JSON NOT NULL, mapping JSON NOT NULL, CONSTRAINT pk_ldap_configuration_id PRIMARY KEY (id))",
        "CREATE TABLE blob_store_configuration (id UUID NOT NULL, name VARCHAR(256) NOT NULL, type VARCHAR(100) NOT NULL,"
            + " attributes JSON NOT NULL, CONSTRAINT pk_blob_store_configuration_id PRIMARY KEY (id),"
            + " CONSTRAINT uk_blob_store_configuration_name UNIQUE (name))",
        "CREATE TABLE repository (id UUID NOT NULL, name VARCHAR(200) NOT NULL, recipe_name VARCHAR(200) NOT NULL,"
            + " online BOOLEAN NOT NULL, routing_rule_id UUID NULL, attributes JSON NOT NULL,"
            + " CONSTRAINT pk_repository_id PRIMARY KEY (id), CONSTRAINT uk_repository_name UNIQUE (name))",
        // After RoutingRuleDAO.xml of the public source, tag release-3.87.2-01 (read 2026-11-19).
        "CREATE TABLE repository_routing_rule (id UUID NOT NULL, name VARCHAR(200) NOT NULL, mode VARCHAR(100) NOT NULL,"
            + " description VARCHAR(400) NULL, matchers JSON NOT NULL,"
            + " CONSTRAINT pk_repository_routing_rule_id PRIMARY KEY (id),"
            + " CONSTRAINT pk_repository_routing_rule_name UNIQUE (name))",
        // The schema list of the old database (tools/h2export/README.md, S6).
        "CREATE TABLE cleanup_policy (name VARCHAR(255) NOT NULL, notes VARCHAR(400) NULL, format VARCHAR(255) NOT NULL,"
            + " mode VARCHAR(255) NOT NULL, criteria JSON NOT NULL, CONSTRAINT pk_cleanup_policy_name PRIMARY KEY (name))",
        "CREATE TABLE capability_storage_item (id UUID NOT NULL, version INT NOT NULL, type VARCHAR(100) NOT NULL,"
            + " enabled BOOLEAN NOT NULL, notes VARCHAR(400) NULL, properties JSON NOT NULL,"
            + " CONSTRAINT pk_capability_storage_item_id PRIMARY KEY (id))",
        "CREATE TABLE qrtz_job_details (sched_name VARCHAR(120) NOT NULL, job_name VARCHAR(200) NOT NULL,"
            + " job_group VARCHAR(200) NOT NULL, description VARCHAR(1000000000) NULL, job_class_name VARCHAR(250) NOT NULL,"
            + " is_durable BOOLEAN NOT NULL, is_nonconcurrent BOOLEAN NOT NULL, is_update_data BOOLEAN NOT NULL,"
            + " requests_recovery BOOLEAN NOT NULL, job_data VARBINARY(1000000000) NULL,"
            + " PRIMARY KEY (sched_name, job_name, job_group))",
        "CREATE TABLE qrtz_triggers (sched_name VARCHAR(120) NOT NULL, trigger_name VARCHAR(200) NOT NULL,"
            + " trigger_group VARCHAR(200) NOT NULL, job_name VARCHAR(200) NOT NULL, job_group VARCHAR(200) NOT NULL,"
            + " description VARCHAR(1000000000) NULL, next_fire_time BIGINT NULL, prev_fire_time BIGINT NULL,"
            + " priority INT NULL, trigger_state VARCHAR(16) NOT NULL, trigger_type VARCHAR(8) NOT NULL,"
            + " start_time BIGINT NOT NULL, end_time BIGINT NULL, calendar_name VARCHAR(200) NULL, misfire_instr SMALLINT NULL,"
            + " job_data VARBINARY(1000000000) NULL, PRIMARY KEY (sched_name, trigger_name, trigger_group))",
        // Two tables the export must never read; their canaries prove it.
        "CREATE TABLE secrets (id INT NOT NULL PRIMARY KEY, purpose VARCHAR(100), key_id VARCHAR(100), secret VARCHAR(400))",
        "CREATE TABLE api_key_v2 (username VARCHAR(200), domain VARCHAR(200), access_key VARCHAR(200), secret VARCHAR(400))",
    };

    // The tables of the secret store in the shape of the old product, for --secret-store.
    static final String[] SECRET_STORE = {
        "DROP TABLE secrets",
        "DROP TABLE api_key_v2",
        "CREATE TABLE secrets (id INT NOT NULL PRIMARY KEY, purpose VARCHAR(200) NOT NULL, key_id VARCHAR(200) NULL,"
            + " secret VARCHAR(800) NOT NULL)",
        "CREATE TABLE api_key_v2 (username VARCHAR(200) NOT NULL, principals BINARY VARYING NOT NULL,"
            + " domain VARCHAR(200) NOT NULL, access_key VARCHAR(200) NOT NULL, secret VARCHAR(200) NOT NULL,"
            + " created TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,"
            + " CONSTRAINT pk_api_key_v2 PRIMARY KEY (username, domain, principals),"
            + " CONSTRAINT uk_api_key_v2_domain_access_key UNIQUE (domain, access_key))",
    };

    static final Map<String, String> REPLACE = new LinkedHashMap<>();

    public static void main(String[] args) throws Exception {
        boolean missingColumn = args.length == 3 && args[1].equals("--missing-column");
        boolean secretStore = args.length == 3 && args[1].equals("--secret-store");
        if (args.length != 1 && !missingColumn && !secretStore) {
            System.err.println("usage: FixtureDb.java <base> [--missing-column TABLE.COLUMN | --secret-store <rows>]");
            System.exit(2);
        }
        List<String[]> store = List.of();
        if (secretStore) {
            store = new java.util.ArrayList<>();
            for (String line : Files.readAllLines(Path.of(args[2]), StandardCharsets.UTF_8)) {
                if (line.isEmpty()) continue;
                String[] f = line.split("\t", -1);
                if (f[0].equals("replace")) REPLACE.put(f[1], f[2]);
                else store.add(f);
            }
        }
        String base = args[0];
        if (Files.exists(Path.of(base + ".mv.db"))) {
            System.err.println("the fixture database exists already");
            System.exit(3);
        }
        String missing = missingColumn ? args[2] : null;
        try (Connection c = DriverManager.getConnection("jdbc:h2:file:" + base + ";TRACE_LEVEL_FILE=0", "", "")) {
            try (Statement s = c.createStatement()) {
                for (String ddl : SCHEMA) s.execute(ddl);
                if (secretStore) {
                    for (String ddl : SECRET_STORE) s.execute(ddl);
                }
            }
            users(c, true);
            roles(c);
            privileges(c);
            settings(c);
            ldap(c);
            blobStores(c);
            routingRules(c);
            repositories(c);
            cleanupPolicies(c);
            capabilities(c);
            tasks(c);
            if (secretStore) secretStore(c, store);
            else canaries(c);
            if (missing != null) {
                // after the rows, so that every table can lose any column
                String[] parts = missing.split("\\.");
                try (Statement s = c.createStatement()) {
                    s.execute("ALTER TABLE " + parts[0] + " DROP COLUMN " + parts[1]);
                }
            }
        }
        System.out.println("fixture database written: " + Path.of(base).getFileName() + ".mv.db");
    }

    static String shiro1(String user, String password) throws Exception {
        byte[] salt = Arrays.copyOf(MessageDigest.getInstance("SHA-256")
            .digest(("fixture salt " + user).getBytes(StandardCharsets.UTF_8)), 16);
        MessageDigest sha = MessageDigest.getInstance("SHA-512");
        sha.update(salt);
        byte[] h = sha.digest(password.getBytes(StandardCharsets.UTF_8));
        for (int i = 1; i < 1024; i++) h = sha.digest(h);
        Base64.Encoder b64 = Base64.getEncoder();
        return "$shiro1$SHA-512$1024$" + b64.encodeToString(salt) + "$" + b64.encodeToString(h);
    }

    static byte[] json(String text) {
        String out = text.replace('\'', '"');
        for (Map.Entry<String, String> e : REPLACE.entrySet()) out = out.replace(e.getKey(), e.getValue());
        return out.getBytes(StandardCharsets.UTF_8);
    }

    static void users(Connection c, boolean withEmail) throws Exception {
        String[][] rows = {
            {"admin", "Ada", "Admin", "active", "admin@example.com"},
            {"viewer", "Vera", "Viewer", "active", "viewer@example.com"},
            {"maven-lead", "Max", "Lead", "active", "maven-lead@example.com"},
            {"locked.user", "Lou", "Locked", "locked", "locked.user@example.com"},
        };
        String sql = withEmail
            ? "INSERT INTO security_user (id, first_name, last_name, password, status, email) VALUES (?, ?, ?, ?, ?, ?)"
            : "INSERT INTO security_user (id, first_name, last_name, password, status) VALUES (?, ?, ?, ?, ?)";
        try (PreparedStatement p = c.prepareStatement(sql)) {
            for (String[] r : rows) {
                p.setString(1, r[0]);
                p.setString(2, r[1]);
                p.setString(3, r[2]);
                p.setString(4, shiro1(r[0], PASSWORDS.get(r[0])));
                p.setString(5, r[3]);
                if (withEmail) p.setString(6, r[4]);
                p.executeUpdate();
            }
        }
        String[][] mappings = {
            {"admin", "default", "['nx-admin']"},
            {"viewer", "default", "['demo-viewer']"},
            {"maven-lead", "default", "['demo-maven-lead']"},
            {"locked.user", "default", "['demo-viewer']"},
            {"ldap.person", "LDAP", "['demo-ops','developers']"},
        };
        try (PreparedStatement p = c.prepareStatement(
                "INSERT INTO user_role_mapping (user_id, user_lo, source, roles) VALUES (?, ?, ?, ?)")) {
            for (String[] m : mappings) {
                p.setString(1, m[0]);
                p.setString(2, m[0].toLowerCase());
                p.setString(3, m[1]);
                p.setBytes(4, json(m[2]));
                p.executeUpdate();
            }
        }
    }

    static void roles(Connection c) throws Exception {
        Object[][] rows = {
            {"nx-admin", "nx-admin", "Administrator Role", "['nx-all']", "[]", true},
            {"demo-viewer", "Demo viewer", "Reads and browses every repository",
                "['nx-repository-view-*-*-read','nx-repository-view-*-*-browse','nx-search-read']", "[]", false},
            {"demo-maven-lead", "Demo maven lead", "Writes to the Maven repositories",
                "['demo-maven-write','demo-csel-maven']", "['demo-viewer']", false},
            {"developers", "developers", "Named like a directory group", "['demo-wildcard-raw','demo-settings-read']",
                "['demo-viewer']", false},
            {"demo-ops", "Demo operations", null, "['demo-npm-admin','demo-script-run']", "['missing-role']", false},
        };
        try (PreparedStatement p = c.prepareStatement(
                "INSERT INTO role (id, name, description, privileges, roles, read_only) VALUES (?, ?, ?, ?, ?, ?)")) {
            for (Object[] r : rows) {
                p.setString(1, (String) r[0]);
                p.setString(2, (String) r[1]);
                if (r[2] == null) p.setNull(3, Types.VARCHAR); else p.setString(3, (String) r[2]);
                p.setBytes(4, json((String) r[3]));
                p.setBytes(5, json((String) r[4]));
                p.setBoolean(6, (Boolean) r[5]);
                p.executeUpdate();
            }
        }
    }

    static void privileges(Connection c) throws Exception {
        String[][] rows = {
            {"demo-wildcard-raw", "wildcard", "Everything on demo-raw", "{'pattern':'nexus:repository-view:raw:demo-raw:*'}"},
            {"demo-settings-read", "application", "Read the settings", "{'domain':'settings','actions':'read'}"},
            {"demo-maven-write", "repository-view", "Add and edit in Maven repositories",
                "{'format':'maven2','repository':'*','actions':'add,edit'}"},
            {"demo-npm-admin", "repository-admin", "Administer demo-npm-proxy",
                "{'format':'npm','repository':'demo-npm-proxy','actions':'*'}"},
            {"demo-csel-maven", "repository-content-selector", "Selected Maven content",
                "{'contentSelector':'demo-selector','repository':'*-maven2','actions':'read,browse'}"},
            {"demo-script-run", "script", "Run the demo script", "{'name':'demo-script','actions':'browse,read,run'}"},
        };
        try (PreparedStatement p = c.prepareStatement(
                "INSERT INTO privilege (id, name, description, type, properties, read_only) VALUES (?, ?, ?, ?, ?, FALSE)")) {
            for (String[] r : rows) {
                p.setString(1, r[0]);
                p.setString(2, r[0]);
                p.setString(3, r[2]);
                p.setString(4, r[1]);
                p.setBytes(5, json(r[3]));
                p.executeUpdate();
            }
        }
    }

    static void settings(Connection c) throws Exception {
        try (PreparedStatement p = c.prepareStatement("INSERT INTO realm_configuration (id, realm_names) VALUES (1, ?)")) {
            p.setBytes(1, json("['NexusAuthenticatingRealm','NexusAuthorizingRealm','LdapRealm','DockerToken']"));
            p.executeUpdate();
        }
        try (Statement s = c.createStatement()) {
            s.execute("INSERT INTO anonymous_configuration (id, enabled, user_id, realm_name)"
                + " VALUES (1, FALSE, 'anonymous', 'NexusAuthorizingRealm')");
        }
    }

    // The two JSON columns in the shape the old product stores them (key
    // names and types of a real row, 2026-11-18): the host as an object, the
    // field names of its stored entity, no group type, and a null member of
    // attribute for static groups.
    static void ldap(Connection c) throws Exception {
        String connection = "{'host':{'protocol':'ldap','hostName':'ldap.example.com','port':389},"
            + "'searchBase':'dc=example,dc=com','systemUsername':'cn=reader,dc=example,dc=com',"
            + "'systemPassword':'CANARY-LDAP-BIND-5d21','authScheme':'SIMPLE','saslRealm':'',"
            + "'connectionTimeout':30,'connectionRetryDelay':300,'maxIncidentsCount':3,'useTrustStore':false}";
        String mapping = "{'emailAddressAttribute':'mail','ldapFilter':'','userBaseDn':'ou=people','userSubtree':true,"
            + "'userObjectClass':'inetOrgPerson','userIdAttribute':'uid','userRealNameAttribute':'cn',"
            + "'userMemberOfAttribute':null,'ldapGroupsAsRoles':true,'groupBaseDn':'ou=groups','groupSubtree':false,"
            + "'groupObjectClass':'groupOfUniqueNames','groupIdAttribute':'cn','groupMemberAttribute':'uniqueMember',"
            + "'groupMemberFormat':'uid=${username},ou=people,dc=example,dc=com','userPasswordAttribute':''}";
        try (PreparedStatement p = c.prepareStatement(
                "INSERT INTO ldap_configuration (id, name, \"order\", connection, mapping) VALUES (?, ?, 1, ?, ?)")) {
            p.setObject(1, java.util.UUID.fromString("6f1c2a52-0000-4000-8000-000000000001"));
            p.setString(2, "demo-directory");
            p.setBytes(3, json(connection));
            p.setBytes(4, json(mapping));
            p.executeUpdate();
        }
    }

    // A store without a soft quota carries an empty blobStoreQuotaConfig, as
    // the old product stores it (2026-11-18).
    static void blobStores(Connection c) throws Exception {
        String[][] rows = {
            {"0b1e0000-0000-4000-8000-000000000001", "demo-files", "File",
                "{'file':{'path':'/srv/blobs/demo-files'},'blobStoreQuotaConfig':{}}"},
            {"0b1e0000-0000-4000-8000-000000000002", "demo-cache", "File",
                "{'file':{'path':'/srv/blobs/demo-cache'},"
                    + "'blobStoreQuotaConfig':{'quotaType':'spaceRemainingQuota','quotaLimitBytes':5368709120}}"},
            {"0b1e0000-0000-4000-8000-000000000003", "demo-s3", "S3",
                "{'s3':{'bucket':'demo-bucket','region':'eu-central-1','accessKeyId':'CANARY-S3-ACCESS-11aa',"
                    + "'secretAccessKey':'CANARY-S3-SECRET-22bb','sessionToken':'CANARY-S3-SESSION-33cc','expiration':'3'},"
                    + "'blobStoreQuotaConfig':{}}"},
            {"0b1e0000-0000-4000-8000-000000000004", "demo-group", "Group",
                "{'group':{'members':['demo-files','demo-cache'],'fillPolicy':'roundRobinFillPolicy'},"
                    + "'blobStoreQuotaConfig':{}}"},
        };
        try (PreparedStatement p = c.prepareStatement(
                "INSERT INTO blob_store_configuration (id, name, type, attributes) VALUES (?, ?, ?, ?)")) {
            for (String[] r : rows) {
                p.setObject(1, java.util.UUID.fromString(r[0]));
                p.setString(2, r[1]);
                p.setString(3, r[2]);
                p.setBytes(4, json(r[3]));
                p.executeUpdate();
            }
        }
    }

    // Three invented routing rules: one of each mode, and one with a look ahead, which Java compiles and Go does
    // not. Only demo-maven-proxy names a rule, and its matchers touch no path another suite uses.
    static final String ROUTED_RULE = "0a0e0000-0000-4000-8000-000000000001";

    static void routingRules(Connection c) throws Exception {
        String[][] rows = {
            {ROUTED_RULE, "demo-block-routed", "BLOCK", "Keeps two invented names off the remote",
                "['^/com/example/routing-blocked/.*','^/@demo-routing-blocked/.*']"},
            {"0a0e0000-0000-4000-8000-000000000002", "demo-allow-releases", "ALLOW",
                "Only the released library and the metadata",
                "['^/com/example/demo-lib/[0-9.]+/.*','.*/maven-metadata\\\\.xml']"},
            {"0a0e0000-0000-4000-8000-000000000003", "demo-java-only", "BLOCK",
                "A look ahead, which Java compiles and Go does not", "['^/com/example/(?!demo-).*']"},
        };
        try (PreparedStatement p = c.prepareStatement(
                "INSERT INTO repository_routing_rule (id, name, mode, description, matchers) VALUES (?, ?, ?, ?, ?)")) {
            for (String[] r : rows) {
                p.setObject(1, java.util.UUID.fromString(r[0]));
                p.setString(2, r[1]);
                p.setString(3, r[2]);
                p.setString(4, r[3]);
                p.setBytes(5, json(r[4]));
                p.executeUpdate();
            }
        }
    }

    static void repositories(Connection c) throws Exception {
        Object[][] rows = {
            {"0e900000-0000-4000-8000-000000000001", "demo-maven-hosted", "maven2-hosted", true,
                "{'storage':{'blobStoreName':'demo-files','strictContentTypeValidation':true,'writePolicy':'ALLOW_ONCE'},"
                    + "'maven':{'versionPolicy':'RELEASE','layoutPolicy':'STRICT'}}"},
            {"0e900000-0000-4000-8000-000000000002", "demo-maven-proxy", "maven2-proxy", true,
                "{'storage':{'blobStoreName':'demo-cache','strictContentTypeValidation':true},"
                    + "'maven':{'versionPolicy':'RELEASE','layoutPolicy':'PERMISSIVE'},"
                    + "'proxy':{'remoteUrl':'https://repo.example.com/maven2/','contentMaxAge':-1,'metadataMaxAge':1440},"
                    + "'httpclient':{'blocked':false,'autoBlock':true,'connection':{'retries':2,'timeout':60},"
                    + "'authentication':{'type':'username','preemptive':false,'username':'demo-proxy-user',"
                    + "'password':'CANARY-PROXY-PASSWORD-7e44'}},"
                    + "'negativeCache':{'enabled':true,'timeToLive':1440}}"},
            {"0e900000-0000-4000-8000-000000000003", "demo-maven-group", "maven2-group", true,
                "{'storage':{'blobStoreName':'demo-files','strictContentTypeValidation':true},"
                    + "'group':{'memberNames':['demo-maven-proxy','demo-maven-hosted']}}"},
            {"0e900000-0000-4000-8000-000000000004", "demo-npm-proxy", "npm-proxy", true,
                "{'storage':{'blobStoreName':'demo-cache','strictContentTypeValidation':true},"
                    + "'proxy':{'remoteUrl':'https://registry.example.com/','contentMaxAge':1440,'metadataMaxAge':1440},"
                    + "'httpclient':{'blocked':false,'autoBlock':true,"
                    + "'authentication':{'type':'bearerToken','preemptive':false,'bearerToken':'CANARY-NPM-BEARER-9f55'}}}"},
            // The old product stores the Docker port with a decimal point (2026-11-18).
            {"0e900000-0000-4000-8000-000000000005", "demo-docker", "docker-hosted", false,
                "{'storage':{'blobStoreName':'demo-files','strictContentTypeValidation':true,'writePolicy':'ALLOW'},"
                    + "'docker':{'httpPort':14511.0,'forceBasicAuth':true,'v1Enabled':false}}"},
            {"0e900000-0000-4000-8000-000000000006", "demo-raw", "raw-hosted", true,
                "{'storage':{'blobStoreName':'demo-s3','strictContentTypeValidation':false,'writePolicy':'ALLOW'},"
                    + "'raw':{'contentDisposition':'ATTACHMENT'}}"},
        };
        try (PreparedStatement p = c.prepareStatement(
                "INSERT INTO repository (id, name, recipe_name, online, routing_rule_id, attributes) VALUES (?, ?, ?, ?, ?, ?)")) {
            for (Object[] r : rows) {
                p.setObject(1, java.util.UUID.fromString((String) r[0]));
                p.setString(2, (String) r[1]);
                p.setString(3, (String) r[2]);
                p.setBoolean(4, (Boolean) r[3]);
                if ("demo-maven-proxy".equals(r[1])) {
                    p.setObject(5, java.util.UUID.fromString(ROUTED_RULE));
                } else {
                    p.setNull(5, Types.OTHER);
                }
                p.setBytes(6, json((String) r[4]));
                p.executeUpdate();
            }
        }
    }

    // Three invented cleanup policies: demo-mixed holds the criterion regex, which this server does not know.
    static void cleanupPolicies(Connection c) throws Exception {
        String[][] rows = {
            {"demo-age", "Invented: components older than 30 days", "maven2", "delete", "{'lastBlobUpdatedDays':30}"},
            {"demo-mixed", null, "ALL_FORMATS", "delete", "{'lastDownloadedDays':60,'regex':'.*-SNAPSHOT.*'}"},
            {"demo-unused", "Invented: never downloaded in 90 days", "npm", "delete", "{'lastDownloadedDays':90}"},
        };
        try (PreparedStatement p = c.prepareStatement(
                "INSERT INTO cleanup_policy (name, notes, format, mode, criteria) VALUES (?, ?, ?, ?, ?)")) {
            for (String[] r : rows) {
                p.setString(1, r[0]);
                p.setString(2, r[1]);
                p.setString(3, r[2]);
                p.setString(4, r[3]);
                p.setBytes(5, json(r[4]));
                p.executeUpdate();
            }
        }
    }

    // Three invented capabilities; demo.webhook holds a secret the export removes.
    static void capabilities(Connection c) throws Exception {
        Object[][] rows = {
            {"0ca00000-0000-4000-8000-000000000001", "baseurl", true, null, "{'url':'https://registry.example.com/'}"},
            {"0ca00000-0000-4000-8000-000000000002", "demo.webhook", false, "Invented hook",
                "{'url':'https://hooks.example.com/demo','secret':'CANARY-CAPABILITY-SECRET-6a1f'}"},
            {"0ca00000-0000-4000-8000-000000000003", "defaultrole", true, null, "{'role':'demo-viewer'}"},
        };
        try (PreparedStatement p = c.prepareStatement(
                "INSERT INTO capability_storage_item (id, version, type, enabled, notes, properties) VALUES (?, 1, ?, ?, ?, ?)")) {
            for (Object[] r : rows) {
                p.setObject(1, java.util.UUID.fromString((String) r[0]));
                p.setString(2, (String) r[1]);
                p.setBoolean(3, (Boolean) r[2]);
                p.setString(4, (String) r[3]);
                p.setBytes(5, json((String) r[4]));
                p.executeUpdate();
            }
        }
    }

    // Six invented tasks of the scheduler. The job data of a job holds the task configuration, that of its
    // trigger the schedule (S10). demo-weekly is written in the form of the scheduler's map classes, the others
    // as a java.util.HashMap; demo-unreadable holds a list, which the export does not read. demo-stricter has
    // a cron expression this server refuses, demo-monthly the last day of the month (999).
    static void tasks(Connection c) throws Exception {
        String[][] jobs = {
            // name, enabled, type, property key, property value, schedule as key=value pairs separated by |
            {"demo-cron", "true", "blobstore.compact", "blobstoreName", "demo-files",
                "schedule.type=cron|schedule.startAt=2026-01-01T00:00:00.000Z|schedule.cronExpression=0 0 2 * * ?"},
            {"demo-manual", "true", "db.backup", "location", "/srv/backup/demo",
                "schedule.type=manual"},
            {"demo-monthly", "true", "repository.cleanup", "repositoryName", "demo-maven-hosted",
                "schedule.type=monthly|schedule.startAt=2026-02-01T04:00:00.000Z|schedule.daysToRun=1,999"},
            {"demo-stricter", "true", "repository.cleanup", "repositoryName", "*",
                "schedule.type=cron|schedule.startAt=2026-01-01T00:00:00.000Z|schedule.cronExpression=0 0 12 15W,20 * ?"},
            {"demo-weekly", "false", "repository.maven.remove-snapshots", "remotePassword", "CANARY-TASK-PASSWORD-4c8e",
                "schedule.type=weekly|schedule.startAt=2026-11-02T03:00:00.000+01:00|schedule.daysToRun=FRI,MON"},
        };
        try (PreparedStatement job = c.prepareStatement("INSERT INTO qrtz_job_details VALUES"
                + " ('nexus', ?, 'demo', NULL, 'demo.TaskJob', TRUE, TRUE, TRUE, FALSE, ?)");
             PreparedStatement trigger = c.prepareStatement("INSERT INTO qrtz_triggers VALUES"
                + " ('nexus', ?, 'demo', ?, 'demo', NULL, NULL, NULL, 5, ?, ?, 1767225600000, NULL, NULL, 0, ?)")) {
            for (String[] r : jobs) {
                java.util.HashMap<String, String> config = new java.util.HashMap<>();
                config.put(".id", r[0]);
                config.put(".name", "Invented task " + r[0]);
                config.put(".typeId", r[2]);
                config.put(".enabled", r[1]);
                config.put(r[3], r[4]);
                config.put("lastRunState.endState", "OK");
                java.util.HashMap<String, String> schedule = new java.util.HashMap<>();
                for (String pair : r[5].split("\\|")) {
                    String[] kv = pair.split("=", 2);
                    schedule.put(kv[0], kv[1]);
                }
                boolean wrapped = r[0].equals("demo-weekly");
                job.setString(1, r[0]);
                job.setBytes(2, wrapped ? jobDataMap(config) : serialized(config));
                job.executeUpdate();
                trigger.setString(1, r[0] + "-trigger");
                trigger.setString(2, r[0]);
                trigger.setString(3, r[1].equals("true") ? "WAITING" : "PAUSED");
                trigger.setString(4, schedule.get("schedule.type").equals("cron") ? "CRON" : "SIMPLE");
                trigger.setBytes(5, wrapped ? jobDataMap(schedule) : serialized(schedule));
                trigger.executeUpdate();
            }
            job.setString(1, "demo-unreadable");
            job.setBytes(2, serialized(new java.util.ArrayList<>(java.util.List.of(".id", "demo-unreadable"))));
            job.executeUpdate();
            trigger.setString(1, "demo-unreadable-trigger");
            trigger.setString(2, "demo-unreadable");
            trigger.setString(3, "WAITING");
            trigger.setString(4, "SIMPLE");
            trigger.setBytes(5, serialized(new java.util.HashMap<>(Map.of("schedule.type", "manual"))));
            trigger.executeUpdate();
        }
    }

    static byte[] serialized(java.io.Serializable value) throws Exception {
        java.io.ByteArrayOutputStream bytes = new java.io.ByteArrayOutputStream();
        try (java.io.ObjectOutputStream out = new java.io.ObjectOutputStream(bytes)) {
            out.writeObject(value);
        }
        return bytes.toByteArray();
    }

    // The stream the Java serialization protocol gives for a job data map of the scheduler (JobDataMap, its
    // parents StringKeyDirtyFlagMap and DirtyFlagMap, and the HashMap in the field map), written by hand as the
    // scheduler's classes are not on this class path. Entries go in key order.
    static byte[] jobDataMap(Map<String, String> entries) throws Exception {
        java.io.ByteArrayOutputStream bytes = new java.io.ByteArrayOutputStream();
        java.io.DataOutputStream o = new java.io.DataOutputStream(bytes);
        o.writeShort(0xACED);
        o.writeShort(5);
        o.writeByte(0x73);
        classDesc(o, "org.quartz.JobDataMap", 0x02);
        o.writeShort(0);
        o.writeByte(0x78);
        classDesc(o, "org.quartz.utils.StringKeyDirtyFlagMap", 0x02);
        o.writeShort(1);
        o.writeByte('Z');
        o.writeUTF("allowsTransientData");
        o.writeByte(0x78);
        classDesc(o, "org.quartz.utils.DirtyFlagMap", 0x02);
        o.writeShort(2);
        o.writeByte('Z');
        o.writeUTF("dirty");
        o.writeByte('L');
        o.writeUTF("map");
        o.writeByte(0x74);
        o.writeUTF("Ljava/util/Map;");
        o.writeByte(0x78);
        o.writeByte(0x70);
        o.writeByte(1);
        o.writeByte(0x73);
        classDesc(o, "java.util.HashMap", 0x03);
        o.writeShort(2);
        o.writeByte('F');
        o.writeUTF("loadFactor");
        o.writeByte('I');
        o.writeUTF("threshold");
        o.writeByte(0x78);
        o.writeByte(0x70);
        o.writeFloat(0.75f);
        o.writeInt(12);
        o.writeByte(0x77);
        o.writeByte(8);
        o.writeInt(16);
        o.writeInt(entries.size());
        for (Map.Entry<String, String> e : new java.util.TreeMap<>(entries).entrySet()) {
            o.writeByte(0x74);
            o.writeUTF(e.getKey());
            o.writeByte(0x74);
            o.writeUTF(e.getValue());
        }
        o.writeByte(0x78);
        o.writeByte(0);
        o.flush();
        return bytes.toByteArray();
    }

    static void classDesc(java.io.DataOutputStream o, String name, int flags) throws Exception {
        o.writeByte(0x72);
        o.writeUTF(name);
        o.writeLong(name.hashCode());
        o.writeByte(flags);
    }

    static void canaries(Connection c) throws Exception {
        try (Statement s = c.createStatement()) {
            s.execute("INSERT INTO secrets VALUES (1, 'ldap', NULL, 'CANARY-SECRETS-TABLE-41b7')");
            s.execute("INSERT INTO api_key_v2 VALUES ('admin', 'NpmToken', 'demo', 'CANARY-API-KEY-TABLE-93c2')");
        }
    }

    static void secretStore(Connection c, List<String[]> rows) throws Exception {
        try (PreparedStatement secret = c.prepareStatement("INSERT INTO secrets VALUES (?, ?, ?, ?)");
             PreparedStatement key = c.prepareStatement(
                 "INSERT INTO api_key_v2 (username, principals, domain, access_key, secret, created) VALUES (?, ?, ?, ?, ?, ?)")) {
            for (String[] f : rows) {
                switch (f[0]) {
                    case "secret" -> {
                        secret.setInt(1, Integer.parseInt(f[1]));
                        secret.setString(2, f[2]);
                        if (f[3].isEmpty()) secret.setNull(3, Types.VARCHAR); else secret.setString(3, f[3]);
                        secret.setString(4, f[4]);
                        secret.executeUpdate();
                    }
                    case "apikey" -> {
                        Map<String, LinkedHashSet<String>> principals = new LinkedHashMap<>();
                        principals.put(f[2].equals("-") ? "OtherRealm" : f[2], new LinkedHashSet<>(List.of(f[1])));
                        ByteArrayOutputStream bytes = new ByteArrayOutputStream();
                        try (ObjectOutputStream o = new ObjectOutputStream(bytes)) {
                            o.writeObject(principals);
                        }
                        key.setString(1, f[1]);
                        key.setBytes(2, bytes.toByteArray());
                        key.setString(3, f[3]);
                        key.setString(4, f[4]);
                        key.setString(5, f[5]);
                        key.setObject(6, java.time.OffsetDateTime.parse(f[6]));
                        key.executeUpdate();
                    }
                    default -> throw new IllegalArgumentException("unknown row kind " + f[0]);
                }
            }
        }
    }

    private FixtureDb() {}
}
