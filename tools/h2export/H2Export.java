// Export of the identity and configuration tables of the old product's H2
// database as JSON lines, with a manifest of counts and SHA-256 digests.
//
// usage: java -cp h2-2.2.224.jar H2Export.java --database <base> --out <dir>
//            [--set content|identity|all|secrets] [--with-password-hashes] [--with-secrets]
//        java -cp h2-2.2.224.jar H2Export.java --selftest
//
// <base> is the database path without ".mv.db". The file is opened read
// only, with an empty user and password, as the old product opens it. Every
// table has a fixed list of columns; nothing else is read. Secret fields are
// removed here, before a byte is written: every key of a JSON column whose
// name contains password, secret, token, key, passphrase or credential, and
// a JSON column that holds no object or list. The password hash column is
// read only with --with-password-hashes; without it every user row carries
// an empty hash. No value of a row is ever printed, in errors neither.
//
// --with-secrets also reads the key table and the secret store of the old
// product (docs/plans/secrets-import.md, decision K1). It decrypts the
// stored values inside this process with the built in passphrase and writes
// api_key_v2.jsonl (the SHA-256 of each key, never the key),
// remote_secrets.jsonl (the remote passwords and bearer tokens of proxy
// repositories) and ldap_secrets.jsonl (the bind passwords). The two last
// files hold clear values: the export directory has mode 0700 and its files
// 0600, and the run book removes it after the import. Without the option
// neither table is read. The set secrets writes these three files only.
//
// Exit codes: 0 done, 1 failed, 2 usage, 3 refused output directory,
// 4 schema mismatch (a table or column is missing), 5 the database cannot be
// opened or the driver is not 2.2.224. Plain JDBC and the Java standard
// library only; tools/h2export.sh runs it in the pinned container.

import java.io.BufferedOutputStream;
import java.io.IOException;
import java.io.OutputStream;
import java.nio.ByteBuffer;
import java.nio.charset.CharacterCodingException;
import java.nio.charset.CodingErrorAction;
import java.nio.charset.StandardCharsets;
import java.nio.file.DirectoryStream;
import java.nio.file.Files;
import java.nio.file.LinkOption;
import java.nio.file.Path;
import java.nio.file.StandardOpenOption;
import java.nio.file.attribute.BasicFileAttributes;
import java.nio.file.attribute.PosixFilePermissions;
import java.security.DigestOutputStream;
import java.security.MessageDigest;
import java.sql.Connection;
import java.sql.DatabaseMetaData;
import java.sql.DriverManager;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Statement;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Base64;
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;
import java.util.TreeSet;
import java.util.regex.Pattern;
import javax.crypto.Cipher;
import javax.crypto.SecretKeyFactory;
import javax.crypto.spec.IvParameterSpec;
import javax.crypto.spec.PBEKeySpec;
import javax.crypto.spec.SecretKeySpec;

public final class H2Export {
    static final String DRIVER = "2.2.224";
    static final String FORMAT = "pipesmith-registry-h2-export";
    static final int OK = 0, FAILED = 1, USAGE = 2, REFUSED = 3, SCHEMA = 4, CANNOT_OPEN = 5;
    static final List<String> SECRET_WORDS = List.of("password", "secret", "token", "key", "passphrase", "credential");
    static final String IDENTITY = "identity", CONTENT = "content", ALL = "all", SECRETS = "secrets";

    enum Type { TEXT, BOOL, INT, UUID, JSON, HASH, JOBDATA }

    record Column(String name, Type type) {
        String key() { return name.toLowerCase(Locale.ROOT); }
        String quoted() { return "\"" + name + "\""; }
    }

    record Table(String name, String set, String orderBy, List<Column> columns) {
        String file() { return name.toLowerCase(Locale.ROOT) + ".jsonl"; }
    }

    static Column col(String name, Type type) { return new Column(name, type); }

    // Table and column names: the mapper files of the old product's public
    // source and a schema list of the old database (tools/h2export/README.md).
    static final List<Table> TABLES = List.of(
        new Table("SECURITY_USER", IDENTITY, "\"ID\"", List.of(
            col("ID", Type.TEXT), col("FIRST_NAME", Type.TEXT), col("LAST_NAME", Type.TEXT),
            col("EMAIL", Type.TEXT), col("STATUS", Type.TEXT), col("PASSWORD", Type.HASH))),
        new Table("ROLE", IDENTITY, "\"ID\"", List.of(
            col("ID", Type.TEXT), col("NAME", Type.TEXT), col("DESCRIPTION", Type.TEXT),
            col("PRIVILEGES", Type.JSON), col("ROLES", Type.JSON), col("READ_ONLY", Type.BOOL))),
        new Table("USER_ROLE_MAPPING", IDENTITY, "\"USER_ID\", \"SOURCE\"", List.of(
            col("USER_ID", Type.TEXT), col("SOURCE", Type.TEXT), col("ROLES", Type.JSON))),
        new Table("PRIVILEGE", IDENTITY, "\"ID\"", List.of(
            col("ID", Type.TEXT), col("NAME", Type.TEXT), col("DESCRIPTION", Type.TEXT), col("TYPE", Type.TEXT),
            col("PROPERTIES", Type.JSON), col("READ_ONLY", Type.BOOL))),
        new Table("REALM_CONFIGURATION", IDENTITY, "\"ID\"", List.of(
            col("ID", Type.INT), col("REALM_NAMES", Type.JSON))),
        new Table("ANONYMOUS_CONFIGURATION", IDENTITY, "\"ID\"", List.of(
            col("ID", Type.INT), col("ENABLED", Type.BOOL), col("USER_ID", Type.TEXT), col("REALM_NAME", Type.TEXT))),
        new Table("LDAP_CONFIGURATION", IDENTITY, "\"order\", \"ID\"", List.of(
            col("ID", Type.UUID), col("NAME", Type.TEXT), col("order", Type.INT),
            col("CONNECTION", Type.JSON), col("MAPPING", Type.JSON))),
        new Table("BLOB_STORE_CONFIGURATION", CONTENT, "\"NAME\"", List.of(
            col("ID", Type.UUID), col("NAME", Type.TEXT), col("TYPE", Type.TEXT), col("ATTRIBUTES", Type.JSON))),
        new Table("REPOSITORY", CONTENT, "\"NAME\"", List.of(
            col("ID", Type.UUID), col("NAME", Type.TEXT), col("RECIPE_NAME", Type.TEXT), col("ONLINE", Type.BOOL),
            col("ROUTING_RULE_ID", Type.UUID), col("ATTRIBUTES", Type.JSON))),
        new Table("REPOSITORY_ROUTING_RULE", CONTENT, "\"NAME\"", List.of(
            col("ID", Type.UUID), col("NAME", Type.TEXT), col("MODE", Type.TEXT), col("DESCRIPTION", Type.TEXT),
            col("MATCHERS", Type.JSON))),
        // Cleanup policies, capabilities and the tasks of the scheduler: the
        // schema list of the old database (S6), the task keys after S10.
        new Table("CLEANUP_POLICY", CONTENT, "\"NAME\"", List.of(
            col("NAME", Type.TEXT), col("NOTES", Type.TEXT), col("FORMAT", Type.TEXT), col("MODE", Type.TEXT),
            col("CRITERIA", Type.JSON))),
        new Table("CAPABILITY_STORAGE_ITEM", CONTENT, "\"ID\"", List.of(
            col("ID", Type.UUID), col("VERSION", Type.INT), col("TYPE", Type.TEXT), col("ENABLED", Type.BOOL),
            col("NOTES", Type.TEXT), col("PROPERTIES", Type.JSON))),
        new Table("QRTZ_JOB_DETAILS", CONTENT, "\"JOB_GROUP\", \"JOB_NAME\"", List.of(
            col("JOB_NAME", Type.TEXT), col("JOB_GROUP", Type.TEXT), col("DESCRIPTION", Type.TEXT),
            col("JOB_CLASS_NAME", Type.TEXT), col("JOB_DATA", Type.JOBDATA))),
        new Table("QRTZ_TRIGGERS", CONTENT, "\"TRIGGER_GROUP\", \"TRIGGER_NAME\"", List.of(
            col("TRIGGER_NAME", Type.TEXT), col("TRIGGER_GROUP", Type.TEXT), col("JOB_NAME", Type.TEXT),
            col("JOB_GROUP", Type.TEXT), col("TRIGGER_STATE", Type.TEXT), col("TRIGGER_TYPE", Type.TEXT),
            col("JOB_DATA", Type.JOBDATA))));

    /** An error with its exit code. The message never holds a value of a row. */
    static final class Exit extends Exception {
        final int code;
        Exit(int code, String message) { super(message, null, false, false); this.code = code; }
    }

    public static void main(String[] args) {
        int code;
        try {
            code = run(args);
        } catch (Exit e) {
            System.err.println("h2export: " + e.getMessage());
            code = e.code;
        } catch (Throwable t) {
            // The class only: a message could carry a value.
            System.err.println("h2export: failed with " + t.getClass().getName());
            code = FAILED;
        }
        System.out.flush();
        System.exit(code);
    }

    static int run(String[] args) throws Exception {
        String database = null, out = null, set = CONTENT;
        boolean hashes = false, selftest = false, secrets = false;
        for (int i = 0; i < args.length; i++) {
            switch (args[i]) {
                case "--database" -> database = value(args, ++i);
                case "--out" -> out = value(args, ++i);
                case "--set" -> set = value(args, ++i);
                case "--with-password-hashes" -> hashes = true;
                case "--with-secrets" -> secrets = true;
                case "--selftest" -> selftest = true;
                default -> throw new Exit(USAGE, "unknown argument " + args[i]);
            }
        }
        if (selftest) {
            if (database != null || out != null) throw new Exit(USAGE, "--selftest takes no other argument");
            return selftest();
        }
        if (database == null || out == null) throw new Exit(USAGE, "--database and --out are required");
        if (!set.equals(CONTENT) && !set.equals(IDENTITY) && !set.equals(ALL) && !set.equals(SECRETS)) {
            throw new Exit(USAGE, "--set is content, identity, all or secrets");
        }
        if (set.equals(SECRETS) && !secrets) throw new Exit(USAGE, "--set secrets needs --with-secrets");
        return export(database, Path.of(out), set, hashes, secrets);
    }

    static String value(String[] args, int i) throws Exit {
        if (i >= args.length) throw new Exit(USAGE, args[i - 1] + " needs a value");
        return args[i];
    }

    static int selftest() throws Exception {
        try (Connection c = DriverManager.getConnection("jdbc:h2:mem:selftest", "", "")) {
            System.out.println("java " + Runtime.version());
            System.out.println("h2 " + c.getMetaData().getDriverVersion());
        }
        return OK;
    }

    record Stamp(long size, long modified) {
        static Stamp of(Path p) throws IOException {
            BasicFileAttributes a = Files.readAttributes(p, BasicFileAttributes.class, LinkOption.NOFOLLOW_LINKS);
            return new Stamp(a.size(), a.lastModifiedTime().toMillis());
        }
    }

    record Written(Table table, List<String> columns, long rows, long removed, String sha256) {}

    static int export(String base, Path out, String set, boolean hashes, boolean secrets) throws Exception {
        Path file = Path.of(base + ".mv.db");
        if (!Files.isRegularFile(file, LinkOption.NOFOLLOW_LINKS)) {
            throw new Exit(CANNOT_OPEN, "the database file does not exist or is not a regular file");
        }
        prepare(out);
        List<Table> tables = new ArrayList<>();
        for (Table t : TABLES) {
            if (set.equals(ALL) || t.set().equals(set)) tables.add(t);
        }
        Stamp before = Stamp.of(file);
        List<Path> created = new ArrayList<>();
        boolean done = false;
        try {
            List<Written> written = new ArrayList<>();
            Left left = null;
            String driver;
            String url = "jdbc:h2:file:" + base
                + ";ACCESS_MODE_DATA=r;IFEXISTS=TRUE;TRACE_LEVEL_FILE=0;TRACE_LEVEL_SYSTEM_OUT=0";
            Connection c;
            try {
                c = DriverManager.getConnection(url, "", "");
            } catch (SQLException e) {
                throw new Exit(CANNOT_OPEN, "cannot open the database: H2 error " + e.getErrorCode()
                    + ", SQL state " + e.getSQLState());
            }
            try (c) {
                c.setReadOnly(true);
                DatabaseMetaData meta = c.getMetaData();
                driver = meta.getDriverVersion();
                if (!driver.equals(DRIVER) && !driver.startsWith(DRIVER + " ")) {
                    throw new Exit(CANNOT_OPEN, "the driver is " + driver + ", not " + DRIVER);
                }
                for (Table t : tables) check(meta, t);
                if (secrets) {
                    for (Table t : Secrets.READ) check(meta, t);
                }
                for (Table t : tables) written.add(dump(c, t, out, hashes, created));
                if (secrets) left = Secrets.export(c, out, created, written);
            } catch (SQLException e) {
                throw new Exit(FAILED, "reading failed: H2 error " + e.getErrorCode() + ", SQL state " + e.getSQLState());
            }
            if (!Stamp.of(file).equals(before)) {
                throw new Exit(FAILED, "the database file changed in size or time during the export");
            }
            writeManifest(out, set, driver, hashes, written, created, left);
            for (Written w : written) {
                System.out.println(w.table().file() + ": " + w.rows() + " rows, " + w.removed() + " fields removed");
            }
            if (left != null) {
                System.out.println("secrets not taken over: " + left.keyed + " under a key of their own, " + left.unreadable
                    + " unreadable, " + left.unknownDomains + " keys of an unknown domain, " + left.unreferenced
                    + " named by nothing");
            }
            System.out.println("export of set " + set + " written: " + written.size() + " files and manifest.json");
            done = true;
            return OK;
        } finally {
            if (!done) {
                for (Path p : created) Files.deleteIfExists(p);
            }
        }
    }

    /** The output directory exists and is empty, with mode 0700. */
    static void prepare(Path out) throws Exception {
        if (Files.isSymbolicLink(out)) throw new Exit(REFUSED, "--out is a symbolic link");
        if (Files.exists(out)) {
            if (!Files.isDirectory(out)) throw new Exit(REFUSED, "--out is not a directory");
            try (DirectoryStream<Path> entries = Files.newDirectoryStream(out)) {
                if (entries.iterator().hasNext()) throw new Exit(REFUSED, "--out is not empty");
            }
        } else {
            Files.createDirectory(out, PosixFilePermissions.asFileAttribute(PosixFilePermissions.fromString("rwx------")));
        }
        Files.setPosixFilePermissions(out, PosixFilePermissions.fromString("rwx------"));
    }

    /** Every listed column exists in table PUBLIC.<name>; else exit 4. */
    static void check(DatabaseMetaData meta, Table t) throws Exception {
        Set<String> present = new LinkedHashSet<>();
        try (ResultSet rs = meta.getColumns(null, "PUBLIC", t.name(), null)) {
            while (rs.next()) {
                if (t.name().equals(rs.getString("TABLE_NAME")) && "PUBLIC".equals(rs.getString("TABLE_SCHEM"))) {
                    present.add(rs.getString("COLUMN_NAME"));
                }
            }
        }
        if (present.isEmpty()) throw new Exit(SCHEMA, "missing table " + t.name());
        for (Column column : t.columns()) {
            if (!present.contains(column.name())) throw new Exit(SCHEMA, "missing column " + t.name() + "." + column.name());
        }
    }

    static OutputStream create(Path p, List<Path> created) throws IOException {
        Files.createFile(p, PosixFilePermissions.asFileAttribute(PosixFilePermissions.fromString("rw-------")));
        created.add(p);
        return Files.newOutputStream(p, StandardOpenOption.WRITE, StandardOpenOption.TRUNCATE_EXISTING);
    }

    static Written dump(Connection c, Table t, Path out, boolean hashes, List<Path> created) throws Exception {
        List<Column> read = new ArrayList<>();
        for (Column column : t.columns()) {
            if (column.type() != Type.HASH || hashes) read.add(column);
        }
        StringBuilder sql = new StringBuilder("SELECT ");
        for (int i = 0; i < read.size(); i++) sql.append(i == 0 ? "" : ", ").append(read.get(i).quoted());
        sql.append(" FROM \"PUBLIC\".\"").append(t.name()).append("\" ORDER BY ").append(t.orderBy());
        List<String> keys = new ArrayList<>();
        for (Column column : t.columns()) keys.add(column.key());
        MessageDigest sha = MessageDigest.getInstance("SHA-256");
        long rows = 0, removedTotal = 0;
        try (Statement s = c.createStatement(); ResultSet rs = s.executeQuery(sql.toString());
             OutputStream raw = create(out.resolve(t.file()), created);
             OutputStream stream = new BufferedOutputStream(new DigestOutputStream(raw, sha))) {
            while (rs.next()) {
                Map<String, Object> row = new LinkedHashMap<>();
                List<String> removed = new ArrayList<>();
                for (Column column : t.columns()) {
                    row.put(column.key(), column.type() == Type.HASH && !hashes ? "" : read(rs, column, removed));
                }
                row.put("_removed", new ArrayList<Object>(removed));
                stream.write(Json.write(row, false).getBytes(StandardCharsets.UTF_8));
                stream.write('\n');
                rows++;
                removedTotal += removed.size();
            }
        }
        return new Written(t, keys, rows, removedTotal, HexFormat.of().formatHex(sha.digest()));
    }

    static Object read(ResultSet rs, Column column, List<String> removed) throws SQLException {
        String label = column.name();
        switch (column.type()) {
            case TEXT, UUID, HASH -> {
                String v = rs.getString(label);
                return v == null ? Json.NULL : v;
            }
            case BOOL -> {
                boolean v = rs.getBoolean(label);
                return rs.wasNull() ? Json.NULL : v;
            }
            case INT -> {
                long v = rs.getLong(label);
                return rs.wasNull() ? Json.NULL : (Object) v;
            }
            case JSON -> {
                byte[] bytes = rs.getBytes(label);
                if (bytes == null) return Json.NULL;
                Object value;
                try {
                    value = Json.parse(utf8(bytes));
                } catch (Json.Invalid | CharacterCodingException e) {
                    removed.add(column.key());
                    return Json.NULL;
                }
                if (!(value instanceof Map) && !(value instanceof List)) {
                    // An encrypted or otherwise unknown form: nothing of it goes out.
                    removed.add(column.key());
                    return Json.NULL;
                }
                return clean(value, column.key(), removed);
            }
            case JOBDATA -> {
                return jobData(rs.getBytes(label), column.key(), removed);
            }
        }
        throw new IllegalStateException("no reader for " + column.type());
    }

    /**
     * A job data column as a JSON object of strings without the secret keys, or null, listed in removed, when
     * JobData cannot read it. An empty column is an empty object.
     */
    static Object jobData(byte[] bytes, String key, List<String> removed) {
        if (bytes == null) return Json.NULL;
        Map<String, Object> map = JobData.decode(bytes);
        if (map == null) {
            removed.add(key);
            return Json.NULL;
        }
        return clean(map, key, removed);
    }

    /**
     * The reader of a job data map of the scheduler (S10): a stream of the Java object serialization protocol
     * that holds a java.util.HashMap of strings, alone or as the field of the scheduler's map classes. The
     * stream is parsed by its grammar, no class is loaded and no object is made. Any other class, a value that
     * is no string, a stream that ends early or has bytes left gives null, and so does a stream nested deeper than
     * MAX_DEPTH, class descriptions and their parents counted, one above MAX_CHARS decoded characters, and one
     * that overflows the stack. Messages never hold a value.
     */
    static final class JobData {
        static final Set<String> MAPS = Set.of("java.util.HashMap");
        static final Set<String> WRAPPERS = Set.of("org.quartz.JobDataMap", "org.quartz.utils.StringKeyDirtyFlagMap",
            "org.quartz.utils.DirtyFlagMap");
        static final int MAX_DEPTH = 16, MAX_ENTRIES = 100_000;
        /** The decoded characters of one row, each repeated reference counted again. */
        static final long MAX_CHARS = 1L << 20;
        static final byte TC_NULL = 0x70, TC_REFERENCE = 0x71, TC_CLASSDESC = 0x72, TC_OBJECT = 0x73,
            TC_STRING = 0x74, TC_BLOCKDATA = 0x77, TC_ENDBLOCKDATA = 0x78;
        static final int SC_WRITE_METHOD = 0x01, SC_SERIALIZABLE = 0x02;

        static final class Refused extends Exception {
            Refused() { super(null, null, false, false); }
        }

        record Field(char type, String name) {}

        record Desc(String name, int flags, List<Field> fields, Desc parent) {}

        private final java.io.DataInputStream in;
        private final List<Object> handles = new ArrayList<>();
        private long chars;

        private JobData(byte[] bytes) { in = new java.io.DataInputStream(new java.io.ByteArrayInputStream(bytes)); }

        static Map<String, Object> decode(byte[] bytes) {
            JobData d = new JobData(bytes);
            try {
                if (d.in.readUnsignedShort() != 0xACED || d.in.readUnsignedShort() != 5) return null;
                Object value = d.content(d.in.readByte(), 0);
                if (d.in.available() != 0 || !(value instanceof Map<?, ?> map)) return null;
                Map<String, Object> out = new java.util.TreeMap<>();
                for (Map.Entry<?, ?> e : map.entrySet()) out.put((String) e.getKey(), e.getValue());
                return out;
            } catch (IOException | Refused | RuntimeException | StackOverflowError e) {
                return null;
            }
        }

        private Object content(byte tc, int depth) throws IOException, Refused {
            if (depth > MAX_DEPTH) throw new Refused();
            switch (tc) {
                case TC_NULL: return null;
                case TC_REFERENCE: {
                    int handle = in.readInt() - 0x7E0000;
                    if (handle < 0 || handle >= handles.size()) throw new Refused();
                    Object value = handles.get(handle);
                    if (value instanceof String text) count(text.length());
                    if (value instanceof Map<?, ?> map) {
                        for (Map.Entry<?, ?> e : map.entrySet()) {
                            count(((String) e.getKey()).length() + ((String) e.getValue()).length());
                        }
                    }
                    return value;
                }
                case TC_STRING: {
                    String text = in.readUTF();
                    count(text.length());
                    handles.add(text);
                    return text;
                }
                case TC_OBJECT: return object(depth);
                default: throw new Refused();
            }
        }

        /** Adds n decoded characters and refuses the row above MAX_CHARS. */
        private void count(long n) throws Refused {
            chars += n;
            if (chars > MAX_CHARS) throw new Refused();
        }

        private Desc classDesc(int depth) throws IOException, Refused {
            if (depth > MAX_DEPTH) throw new Refused();
            byte tc = in.readByte();
            if (tc == TC_NULL) return null;
            if (tc == TC_REFERENCE) {
                int handle = in.readInt() - 0x7E0000;
                if (handle < 0 || handle >= handles.size() || !(handles.get(handle) instanceof Desc desc)) {
                    throw new Refused();
                }
                return desc;
            }
            if (tc != TC_CLASSDESC) throw new Refused();
            String name = in.readUTF();
            if (!MAPS.contains(name) && !WRAPPERS.contains(name)) throw new Refused();
            in.readLong();
            int slot = handles.size();
            handles.add(null);
            int flags = in.readUnsignedByte();
            if ((flags & SC_SERIALIZABLE) == 0) throw new Refused();
            int count = in.readUnsignedShort();
            List<Field> fields = new ArrayList<>();
            for (int i = 0; i < count; i++) {
                char type = (char) in.readUnsignedByte();
                String field = in.readUTF();
                if (type == 'L' || type == '[') {
                    if (!(content(in.readByte(), depth + 1) instanceof String)) throw new Refused();
                } else if ("BCDFIJSZ".indexOf(type) < 0) {
                    throw new Refused();
                }
                fields.add(new Field(type, field));
            }
            if (in.readByte() != TC_ENDBLOCKDATA) throw new Refused();
            Desc desc = new Desc(name, flags, fields, null);
            handles.set(slot, desc);
            Desc parent = classDesc(depth + 1);
            desc = new Desc(name, flags, fields, parent);
            handles.set(slot, desc);
            return desc;
        }

        /** An object: the map of a HashMap, or the one map a wrapper holds in its fields. */
        private Object object(int depth) throws IOException, Refused {
            Desc desc = classDesc(depth + 1);
            if (desc == null) throw new Refused();
            int slot = handles.size();
            handles.add(null);
            List<Desc> chain = new ArrayList<>();
            for (Desc d = desc; d != null; d = d.parent()) chain.add(0, d);
            Map<String, Object> map = null;
            for (Desc d : chain) {
                for (Field f : d.fields()) {
                    Object value = field(f, depth);
                    if (value instanceof Map<?, ?> m) {
                        if (map != null) throw new Refused();
                        @SuppressWarnings("unchecked") Map<String, Object> found = (Map<String, Object>) m;
                        map = found;
                    }
                }
                if ((d.flags() & SC_WRITE_METHOD) != 0) {
                    if (!MAPS.contains(d.name())) throw new Refused();
                    map = entries(depth);
                }
            }
            if (map == null) throw new Refused();
            handles.set(slot, map);
            return map;
        }

        private Object field(Field f, int depth) throws IOException, Refused {
            switch (f.type()) {
                case 'B', 'Z' -> in.readByte();
                case 'C', 'S' -> in.readShort();
                case 'F', 'I' -> in.readInt();
                case 'D', 'J' -> in.readLong();
                default -> { return content(in.readByte(), depth + 1); }
            }
            return null;
        }

        /** The data HashMap.writeObject writes: a block of the bucket count and the size, then the pairs. */
        private Map<String, Object> entries(int depth) throws IOException, Refused {
            if (in.readByte() != TC_BLOCKDATA || in.readUnsignedByte() != 8) throw new Refused();
            in.readInt();
            int size = in.readInt();
            if (size < 0 || size > MAX_ENTRIES) throw new Refused();
            Map<String, Object> map = new LinkedHashMap<>();
            for (int i = 0; i < size; i++) {
                if (!(content(in.readByte(), depth + 1) instanceof String k)) throw new Refused();
                if (!(content(in.readByte(), depth + 1) instanceof String v)) throw new Refused();
                if (map.put(k, v) != null) throw new Refused();
            }
            if (in.readByte() != TC_ENDBLOCKDATA) throw new Refused();
            return map;
        }
    }

    static String utf8(byte[] bytes) throws CharacterCodingException {
        return StandardCharsets.UTF_8.newDecoder().onMalformedInput(CodingErrorAction.REPORT)
            .onUnmappableCharacter(CodingErrorAction.REPORT).decode(ByteBuffer.wrap(bytes)).toString();
    }

    static boolean secret(String key) {
        String lower = key.toLowerCase(Locale.ROOT);
        for (String word : SECRET_WORDS) {
            if (lower.contains(word)) return true;
        }
        return false;
    }

    /** The value without the secret keys of every object in it; their paths go into removed. */
    @SuppressWarnings("unchecked")
    static Object clean(Object value, String path, List<String> removed) {
        if (value instanceof Map<?, ?> map) {
            Map<String, Object> kept = new LinkedHashMap<>();
            for (Map.Entry<String, Object> e : ((Map<String, Object>) map).entrySet()) {
                String child = path + "." + e.getKey();
                if (secret(e.getKey())) {
                    removed.add(child);
                } else {
                    kept.put(e.getKey(), clean(e.getValue(), child, removed));
                }
            }
            return kept;
        }
        if (value instanceof List<?> list) {
            List<Object> kept = new ArrayList<>();
            for (int i = 0; i < list.size(); i++) kept.add(clean(list.get(i), path + "[" + i + "]", removed));
            return kept;
        }
        return value;
    }

    static void writeManifest(Path out, String set, String driver, boolean hashes, List<Written> written,
                              List<Path> created, Left left) throws IOException {
        List<Object> files = new ArrayList<>();
        for (Written w : written) {
            Map<String, Object> entry = new LinkedHashMap<>();
            entry.put("table", w.table().name());
            entry.put("file", w.table().file());
            entry.put("columns", new ArrayList<Object>(w.columns()));
            entry.put("rows", w.rows());
            entry.put("removed", w.removed());
            entry.put("sha256", w.sha256());
            files.add(entry);
        }
        Map<String, Object> manifest = new LinkedHashMap<>();
        manifest.put("format", FORMAT);
        manifest.put("version", 1L);
        manifest.put("set", set);
        manifest.put("driver", driver);
        manifest.put("passwordHashes", hashes);
        manifest.put("files", files);
        if (left != null) {
            Map<String, Object> counts = new LinkedHashMap<>();
            counts.put("keyed", left.keyed);
            counts.put("unreadable", left.unreadable);
            counts.put("unknownDomains", left.unknownDomains);
            counts.put("unreferenced", left.unreferenced);
            manifest.put("secrets", true);
            manifest.put("secretsLeft", counts);
        }
        try (OutputStream stream = create(out.resolve("manifest.json"), created)) {
            stream.write((Json.write(manifest, true) + "\n").getBytes(StandardCharsets.UTF_8));
        }
    }

    /** The counts of what --with-secrets did not take over. */
    static final class Left {
        long keyed, unreadable, unknownDomains, unreferenced;
    }

    /**
     * The secrets of --with-secrets. The scheme is the one of the old
     * product's public source (docs/plans/secrets-import.md, section Sources):
     * a value is a reference _<id> into SECRETS, a PHC string
     * $<alg>[$v=<n>]$iv=<hex>,key_iteration=<n>,key_len=<bits>$<salt>$<ct>, or
     * the legacy form, Base64 of AES/CBC under the passphrase and salt
     * "changeme" and the iv "0123456789ABCDEF". A key is ACCESS_KEY followed
     * by its decrypted second half. No value leaves this class but through
     * the three files.
     */
    static final class Secrets {
        static final String PASSPHRASE = "changeme";
        static final Pattern REFERENCE = Pattern.compile("_[0-9]{1,9}");
        static final Set<String> DOMAINS = Set.of("NpmToken", "DockerToken", "NuGetApiKey");
        static final String REALM_LOCAL = "NexusAuthenticatingRealm", REALM_LDAP = "LdapRealm";

        // The tables read only with --with-secrets, for the schema check.
        static final List<Table> READ = List.of(
            new Table("REPOSITORY", "secrets", "", List.of(col("NAME", Type.TEXT), col("ATTRIBUTES", Type.JSON))),
            new Table("LDAP_CONFIGURATION", "secrets", "", List.of(col("ID", Type.UUID), col("order", Type.INT),
                col("CONNECTION", Type.JSON))),
            new Table("API_KEY_V2", "secrets", "\"DOMAIN\"", List.of(col("USERNAME", Type.TEXT),
                col("PRINCIPALS", Type.TEXT), col("DOMAIN", Type.TEXT), col("ACCESS_KEY", Type.TEXT),
                col("SECRET", Type.TEXT), col("CREATED", Type.TEXT))),
            new Table("SECRETS", "secrets", "\"ID\"", List.of(col("ID", Type.INT), col("KEY_ID", Type.TEXT),
                col("SECRET", Type.TEXT))));
        // The files written, with the columns the import reads.
        static final Table KEYS = new Table("API_KEY_V2", "secrets", "", List.of(col("USERNAME", Type.TEXT),
            col("DOMAIN", Type.TEXT), col("REALM", Type.TEXT), col("SHA256", Type.TEXT), col("CREATED", Type.TEXT)));
        static final Table REMOTE = new Table("REMOTE_SECRETS", "secrets", "", List.of(col("REPOSITORY", Type.TEXT),
            col("FIELD", Type.TEXT), col("SECRET", Type.TEXT)));
        static final Table LDAP = new Table("LDAP_SECRETS", "secrets", "", List.of(col("ID", Type.TEXT),
            col("SECRET", Type.TEXT)));

        /** A value that is not taken over: under a key of its own, or not decryptable. */
        static final class Skip extends Exception {
            final boolean keyed;
            Skip(boolean keyed) { super(null, null, false, false); this.keyed = keyed; }
        }

        record Stored(String keyId, String secret) {}

        static Left export(Connection c, Path out, List<Path> created, List<Written> written) throws Exception {
            Left left = new Left();
            Map<Integer, Stored> store = new TreeMap<>();
            try (Statement s = c.createStatement();
                 ResultSet rs = s.executeQuery("SELECT \"ID\", \"KEY_ID\", \"SECRET\" FROM \"PUBLIC\".\"SECRETS\" ORDER BY \"ID\"")) {
                while (rs.next()) store.put(rs.getInt(1), new Stored(rs.getString(2), rs.getString(3)));
            }
            Set<Integer> referenced = new TreeSet<>();
            written.add(write(out, created, KEYS, keys(c, store, referenced, left)));
            written.add(write(out, created, REMOTE, remote(c, store, referenced, left)));
            written.add(write(out, created, LDAP, ldap(c, store, referenced, left)));
            for (Integer id : store.keySet()) {
                if (!referenced.contains(id)) left.unreferenced++;
            }
            return left;
        }

        static List<Map<String, Object>> keys(Connection c, Map<Integer, Stored> store, Set<Integer> referenced, Left left)
                throws Exception {
            List<Map<String, Object>> rows = new ArrayList<>();
            String sql = "SELECT \"USERNAME\", \"PRINCIPALS\", \"DOMAIN\", \"ACCESS_KEY\", \"SECRET\", \"CREATED\""
                + " FROM \"PUBLIC\".\"API_KEY_V2\" ORDER BY \"DOMAIN\", \"USERNAME\", \"ACCESS_KEY\"";
            try (Statement s = c.createStatement(); ResultSet rs = s.executeQuery(sql)) {
                while (rs.next()) {
                    String domain = rs.getString("DOMAIN"), secret = rs.getString("SECRET");
                    if (!DOMAINS.contains(domain)) {
                        if (secret != null && REFERENCE.matcher(secret).matches()) referenced.add(Integer.parseInt(secret.substring(1)));
                        left.unknownDomains++;
                        continue;
                    }
                    String second;
                    try {
                        second = resolve(secret, store, referenced);
                    } catch (Skip e) {
                        if (e.keyed) left.keyed++; else left.unreadable++;
                        continue;
                    }
                    String access = rs.getString("ACCESS_KEY");
                    byte[] key = ((access == null ? "" : access) + second).getBytes(StandardCharsets.UTF_8);
                    Map<String, Object> row = new LinkedHashMap<>();
                    row.put("username", rs.getString("USERNAME"));
                    row.put("domain", domain);
                    row.put("realm", realm(rs.getBytes("PRINCIPALS")));
                    row.put("sha256", HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(key)));
                    java.time.OffsetDateTime at = rs.getObject("CREATED", java.time.OffsetDateTime.class);
                    row.put("created", at == null ? Json.NULL
                        : at.format(java.time.format.DateTimeFormatter.ISO_OFFSET_DATE_TIME));
                    Arrays.fill(key, (byte) 0);
                    rows.add(row);
                }
            }
            return rows;
        }

        static List<Map<String, Object>> remote(Connection c, Map<Integer, Stored> store, Set<Integer> referenced, Left left)
                throws Exception {
            List<Map<String, Object>> rows = new ArrayList<>();
            String sql = "SELECT \"NAME\", \"ATTRIBUTES\" FROM \"PUBLIC\".\"REPOSITORY\" ORDER BY \"NAME\"";
            try (Statement s = c.createStatement(); ResultSet rs = s.executeQuery(sql)) {
                while (rs.next()) {
                    Object auth = at(json(rs.getBytes("ATTRIBUTES")), "httpclient", "authentication");
                    for (String field : List.of("password", "bearerToken")) {
                        if (!(auth instanceof Map<?, ?> m) || !(m.get(field) instanceof String value) || value.isEmpty()) continue;
                        try {
                            String clear = resolve(value, store, referenced);
                            Map<String, Object> row = new LinkedHashMap<>();
                            row.put("repository", rs.getString("NAME"));
                            row.put("field", field);
                            row.put("secret", clear);
                            rows.add(row);
                        } catch (Skip e) {
                            if (e.keyed) left.keyed++; else left.unreadable++;
                        }
                    }
                }
            }
            return rows;
        }

        static List<Map<String, Object>> ldap(Connection c, Map<Integer, Stored> store, Set<Integer> referenced, Left left)
                throws Exception {
            List<Map<String, Object>> rows = new ArrayList<>();
            String sql = "SELECT \"ID\", \"CONNECTION\" FROM \"PUBLIC\".\"LDAP_CONFIGURATION\" ORDER BY \"order\", \"ID\"";
            try (Statement s = c.createStatement(); ResultSet rs = s.executeQuery(sql)) {
                while (rs.next()) {
                    if (!(at(json(rs.getBytes("CONNECTION")), "systemPassword") instanceof String value) || value.isEmpty()) continue;
                    try {
                        String clear = resolve(value, store, referenced);
                        Map<String, Object> row = new LinkedHashMap<>();
                        row.put("id", rs.getString("ID"));
                        row.put("secret", clear);
                        rows.add(row);
                    } catch (Skip e) {
                        if (e.keyed) left.keyed++; else left.unreadable++;
                    }
                }
            }
            return rows;
        }

        static Object json(byte[] bytes) {
            if (bytes == null) return null;
            try {
                return Json.parse(utf8(bytes));
            } catch (Json.Invalid | CharacterCodingException e) {
                return null;
            }
        }

        static Object at(Object value, String... path) {
            for (String key : path) {
                if (!(value instanceof Map<?, ?> m)) return null;
                value = m.get(key);
            }
            return value;
        }

        /** The realm named in the bytes of a principal collection, found without deserializing them (K3). */
        static String realm(byte[] principals) {
            if (principals == null) return "unknown";
            boolean local = contains(principals, REALM_LOCAL), ldap = contains(principals, REALM_LDAP);
            if (local == ldap) return "unknown";
            return local ? REALM_LOCAL : REALM_LDAP;
        }

        static boolean contains(byte[] hay, String needle) {
            byte[] n = needle.getBytes(StandardCharsets.US_ASCII);
            outer:
            for (int i = 0; i + n.length <= hay.length; i++) {
                for (int j = 0; j < n.length; j++) {
                    if (hay[i + j] != n[j]) continue outer;
                }
                return true;
            }
            return false;
        }

        static String resolve(String value, Map<Integer, Stored> store, Set<Integer> referenced) throws Skip {
            if (value == null) throw new Skip(false);
            if (REFERENCE.matcher(value).matches()) {
                int id = Integer.parseInt(value.substring(1));
                referenced.add(id);
                Stored row = store.get(id);
                if (row == null || row.secret() == null) throw new Skip(false);
                if (row.keyId() != null) throw new Skip(true);
                return decrypt(row.secret());
            }
            return decrypt(value);
        }

        static String decrypt(String value) throws Skip {
            try {
                byte[] plain = value.startsWith("$") ? phc(value) : value.startsWith("{") ? null
                    : aes("PBKDF2WithHmacSHA1", PASSPHRASE.getBytes(StandardCharsets.UTF_8), 1024, 128,
                        "0123456789ABCDEF".getBytes(StandardCharsets.UTF_8), Base64.getDecoder().decode(value.trim()));
                if (plain == null) throw new Skip(false);
                String text = utf8(plain);
                Arrays.fill(plain, (byte) 0);
                return text;
            } catch (Skip e) {
                throw e;
            } catch (Exception e) {
                throw new Skip(false);
            }
        }

        static byte[] phc(String value) throws Exception {
            List<String> parts = new ArrayList<>();
            for (String p : value.trim().split("\\$")) if (!p.isEmpty()) parts.add(p);
            if (parts.size() < 4 || parts.size() > 5) throw new Skip(false);
            int n = parts.size();
            String alg = parts.get(0), attrs = n == 4 ? parts.get(1) : parts.get(2);
            String prf;
            int bits, iterations;
            switch (alg) {
                case "PBKDF2WithHmacSHA256" -> { prf = alg; bits = 256; iterations = 10000; }
                case "PBKDF2WithHmacSHA1", "AES/CBC/PKCS5Padding" -> { prf = "PBKDF2WithHmacSHA1"; bits = 128; iterations = 1024; }
                default -> throw new Skip(false);
            }
            byte[] iv = null;
            for (String a : attrs.split(",")) {
                int eq = a.indexOf('=');
                if (eq < 0) continue;
                String k = a.substring(0, eq).trim(), v = a.substring(eq + 1).trim();
                if (k.equals("iv")) iv = HexFormat.of().parseHex(v);
                else if (k.equals("key_iteration")) iterations = Integer.parseInt(v);
                else if (k.equals("key_len")) bits = Integer.parseInt(v);
            }
            if (iv == null || iterations < 1 || iterations > 10_000_000) throw new Skip(false);
            Base64.Decoder b64 = Base64.getDecoder();
            return aes(prf, b64.decode(parts.get(n - 2)), iterations, bits, iv, b64.decode(parts.get(n - 1)));
        }

        static byte[] aes(String prf, byte[] salt, int iterations, int bits, byte[] iv, byte[] ct) throws Exception {
            PBEKeySpec spec = new PBEKeySpec(PASSPHRASE.toCharArray(), salt, iterations, bits);
            byte[] key = SecretKeyFactory.getInstance(prf).generateSecret(spec).getEncoded();
            spec.clearPassword();
            Cipher cipher = Cipher.getInstance("AES/CBC/PKCS5Padding");
            cipher.init(Cipher.DECRYPT_MODE, new SecretKeySpec(key, "AES"), new IvParameterSpec(iv));
            Arrays.fill(key, (byte) 0);
            return cipher.doFinal(ct);
        }

        static Written write(Path out, List<Path> created, Table t, List<Map<String, Object>> rows) throws Exception {
            MessageDigest sha = MessageDigest.getInstance("SHA-256");
            try (OutputStream raw = create(out.resolve(t.file()), created);
                 OutputStream stream = new BufferedOutputStream(new DigestOutputStream(raw, sha))) {
                for (Map<String, Object> row : rows) {
                    row.put("_removed", new ArrayList<Object>());
                    stream.write(Json.write(row, false).getBytes(StandardCharsets.UTF_8));
                    stream.write('\n');
                }
            }
            List<String> keys = new ArrayList<>();
            for (Column column : t.columns()) keys.add(column.key());
            return new Written(t, keys, rows.size(), 0, HexFormat.of().formatHex(sha.digest()));
        }

        private Secrets() {}
    }

    /** A small JSON reader and writer: objects keep their key order, numbers their text. */
    static final class Json {
        static final Object NULL = new Object() {
            @Override public String toString() { return "null"; }
        };

        record Num(String text) {}

        static final class Invalid extends Exception {
            Invalid() { super(null, null, false, false); }
        }

        private static final int MAX_DEPTH = 256;
        private final String s;
        private int i;

        private Json(String s) { this.s = s; }

        static Object parse(String text) throws Invalid {
            Json p = new Json(text);
            p.space();
            Object v = p.value(0);
            p.space();
            if (p.i != p.s.length()) throw new Invalid();
            return v;
        }

        private void space() {
            while (i < s.length() && " \t\r\n".indexOf(s.charAt(i)) >= 0) i++;
        }

        private char peek() throws Invalid {
            if (i >= s.length()) throw new Invalid();
            return s.charAt(i);
        }

        private void expect(String word) throws Invalid {
            if (!s.startsWith(word, i)) throw new Invalid();
            i += word.length();
        }

        private Object value(int depth) throws Invalid {
            if (depth > MAX_DEPTH) throw new Invalid();
            char ch = peek();
            switch (ch) {
                case '{': return object(depth);
                case '[': return array(depth);
                case '"': return string();
                case 't': expect("true"); return Boolean.TRUE;
                case 'f': expect("false"); return Boolean.FALSE;
                case 'n': expect("null"); return NULL;
                default: return number();
            }
        }

        private Map<String, Object> object(int depth) throws Invalid {
            Map<String, Object> map = new LinkedHashMap<>();
            i++;
            space();
            if (peek() == '}') { i++; return map; }
            while (true) {
                space();
                if (peek() != '"') throw new Invalid();
                String key = string();
                space();
                expect(":");
                space();
                map.put(key, value(depth + 1));
                space();
                char ch = peek();
                i++;
                if (ch == '}') return map;
                if (ch != ',') throw new Invalid();
            }
        }

        private List<Object> array(int depth) throws Invalid {
            List<Object> list = new ArrayList<>();
            i++;
            space();
            if (peek() == ']') { i++; return list; }
            while (true) {
                space();
                list.add(value(depth + 1));
                space();
                char ch = peek();
                i++;
                if (ch == ']') return list;
                if (ch != ',') throw new Invalid();
            }
        }

        private String string() throws Invalid {
            StringBuilder b = new StringBuilder();
            i++;
            while (true) {
                char ch = peek();
                i++;
                if (ch == '"') return b.toString();
                if (ch < 0x20) throw new Invalid();
                if (ch != '\\') { b.append(ch); continue; }
                char esc = peek();
                i++;
                switch (esc) {
                    case '"' -> b.append('"');
                    case '\\' -> b.append('\\');
                    case '/' -> b.append('/');
                    case 'b' -> b.append('\b');
                    case 'f' -> b.append('\f');
                    case 'n' -> b.append('\n');
                    case 'r' -> b.append('\r');
                    case 't' -> b.append('\t');
                    case 'u' -> {
                        if (i + 4 > s.length()) throw new Invalid();
                        try {
                            b.append((char) Integer.parseInt(s.substring(i, i + 4), 16));
                        } catch (NumberFormatException e) {
                            throw new Invalid();
                        }
                        i += 4;
                    }
                    default -> throw new Invalid();
                }
            }
        }

        private Num number() throws Invalid {
            int start = i;
            if (i < s.length() && s.charAt(i) == '-') i++;
            int digits = i;
            while (i < s.length() && s.charAt(i) >= '0' && s.charAt(i) <= '9') i++;
            if (i == digits || (s.charAt(digits) == '0' && i - digits > 1)) throw new Invalid();
            if (i < s.length() && s.charAt(i) == '.') {
                i++;
                int frac = i;
                while (i < s.length() && s.charAt(i) >= '0' && s.charAt(i) <= '9') i++;
                if (i == frac) throw new Invalid();
            }
            if (i < s.length() && (s.charAt(i) == 'e' || s.charAt(i) == 'E')) {
                i++;
                if (i < s.length() && (s.charAt(i) == '+' || s.charAt(i) == '-')) i++;
                int exp = i;
                while (i < s.length() && s.charAt(i) >= '0' && s.charAt(i) <= '9') i++;
                if (i == exp) throw new Invalid();
            }
            return new Num(s.substring(start, i));
        }

        static String write(Object v, boolean indent) {
            StringBuilder b = new StringBuilder();
            write(b, v, indent, 0);
            return b.toString();
        }

        private static void newline(StringBuilder b, boolean indent, int level) {
            if (!indent) return;
            b.append('\n');
            b.append("  ".repeat(level));
        }

        @SuppressWarnings("unchecked")
        private static void write(StringBuilder b, Object v, boolean indent, int level) {
            if (v == NULL || v == null) {
                b.append("null");
            } else if (v instanceof Boolean || v instanceof Long) {
                b.append(v);
            } else if (v instanceof Num n) {
                b.append(n.text());
            } else if (v instanceof String str) {
                quote(b, str);
            } else if (v instanceof Map<?, ?> map) {
                b.append('{');
                boolean first = true;
                for (Map.Entry<String, Object> e : ((Map<String, Object>) map).entrySet()) {
                    if (!first) b.append(',');
                    first = false;
                    newline(b, indent, level + 1);
                    quote(b, e.getKey());
                    b.append(indent ? ": " : ":");
                    write(b, e.getValue(), indent, level + 1);
                }
                if (!first) newline(b, indent, level);
                b.append('}');
            } else if (v instanceof List<?> list) {
                b.append('[');
                for (int k = 0; k < list.size(); k++) {
                    if (k > 0) b.append(',');
                    if (indent && list.get(k) instanceof Map) newline(b, true, level + 1);
                    write(b, list.get(k), indent, level + 1);
                }
                if (indent && !list.isEmpty() && list.get(0) instanceof Map) newline(b, true, level);
                b.append(']');
            } else {
                throw new IllegalStateException("no JSON form for " + v.getClass().getName());
            }
        }

        private static void quote(StringBuilder b, String str) {
            b.append('"');
            for (int k = 0; k < str.length(); k++) {
                char ch = str.charAt(k);
                switch (ch) {
                    case '"' -> b.append("\\\"");
                    case '\\' -> b.append("\\\\");
                    case '\n' -> b.append("\\n");
                    case '\r' -> b.append("\\r");
                    case '\t' -> b.append("\\t");
                    case '\b' -> b.append("\\b");
                    case '\f' -> b.append("\\f");
                    default -> {
                        if (Character.isHighSurrogate(ch) && k + 1 < str.length() && Character.isLowSurrogate(str.charAt(k + 1))) {
                            b.append(ch).append(str.charAt(++k));
                        } else if (ch < 0x20 || Character.isSurrogate(ch)) {
                            b.append(String.format("\\u%04x", (int) ch));
                        } else {
                            b.append(ch);
                        }
                    }
                }
            }
            b.append('"');
        }
    }

    private H2Export() {}
}
