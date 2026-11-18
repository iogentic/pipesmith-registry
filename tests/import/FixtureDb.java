// Builds the fixture database of tests/import: an H2 2.2.224 file with the
// tables of the old product's public schema and invented rows only.
//
// usage: java -cp h2-2.2.224.jar FixtureDb.java <base> [--missing-column TABLE.COLUMN]
//
// <base> is the path without ".mv.db" and must not exist. User and password
// are empty, as the old product opens its embedded file. JSON columns are
// bound as UTF-8 bytes, as the old product's type handler binds them for H2.
// The passwords below are invented and belong to no one; their hashes are
// Shiro 1 hashes (SHA-512, 1024 rounds) with a salt derived from the user id,
// so every build gives the same rows. The CANARY values stand for secrets:
// none of them may reach an export. --missing-column leaves one column out,
// for the schema check of the export.

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
        // Two tables the export must never read; their canaries prove it.
        "CREATE TABLE secrets (id INT NOT NULL PRIMARY KEY, purpose VARCHAR(100), key_id VARCHAR(100), secret VARCHAR(400))",
        "CREATE TABLE api_key_v2 (username VARCHAR(200), domain VARCHAR(200), access_key VARCHAR(200), secret VARCHAR(400))",
    };

    public static void main(String[] args) throws Exception {
        if (args.length != 1 && !(args.length == 3 && args[1].equals("--missing-column"))) {
            System.err.println("usage: FixtureDb.java <base> [--missing-column TABLE.COLUMN]");
            System.exit(2);
        }
        String base = args[0];
        if (Files.exists(Path.of(base + ".mv.db"))) {
            System.err.println("the fixture database exists already");
            System.exit(3);
        }
        String missing = args.length == 3 ? args[2] : null;
        try (Connection c = DriverManager.getConnection("jdbc:h2:file:" + base + ";TRACE_LEVEL_FILE=0", "", "")) {
            try (Statement s = c.createStatement()) {
                for (String ddl : SCHEMA) s.execute(ddl);
                if (missing != null) {
                    String[] parts = missing.split("\\.");
                    s.execute("ALTER TABLE " + parts[0] + " DROP COLUMN " + parts[1]);
                }
            }
            users(c, missing == null);
            roles(c);
            privileges(c);
            settings(c);
            ldap(c);
            blobStores(c);
            repositories(c);
            canaries(c);
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

    static byte[] json(String text) { return text.replace('\'', '"').getBytes(StandardCharsets.UTF_8); }

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
                "INSERT INTO repository (id, name, recipe_name, online, routing_rule_id, attributes) VALUES (?, ?, ?, ?, NULL, ?)")) {
            for (Object[] r : rows) {
                p.setObject(1, java.util.UUID.fromString((String) r[0]));
                p.setString(2, (String) r[1]);
                p.setString(3, (String) r[2]);
                p.setBoolean(4, (Boolean) r[3]);
                p.setBytes(5, json((String) r[4]));
                p.executeUpdate();
            }
        }
    }

    static void canaries(Connection c) throws Exception {
        try (Statement s = c.createStatement()) {
            s.execute("INSERT INTO secrets VALUES (1, 'ldap', NULL, 'CANARY-SECRETS-TABLE-41b7')");
            s.execute("INSERT INTO api_key_v2 VALUES ('admin', 'NpmToken', 'demo', 'CANARY-API-KEY-TABLE-93c2')");
        }
    }

    private FixtureDb() {}
}
