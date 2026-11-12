// Export of the identity and configuration tables of the old product's H2
// database as JSON lines, with a manifest of counts and SHA-256 digests.
//
// usage: java -cp h2-2.2.224.jar H2Export.java --database <base> --out <dir>
//            [--set content|identity|all] [--with-password-hashes]
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
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;

public final class H2Export {
    static final String DRIVER = "2.2.224";
    static final String FORMAT = "pipesmith-registry-h2-export";
    static final int OK = 0, FAILED = 1, USAGE = 2, REFUSED = 3, SCHEMA = 4, CANNOT_OPEN = 5;
    static final List<String> SECRET_WORDS = List.of("password", "secret", "token", "key", "passphrase", "credential");
    static final String IDENTITY = "identity", CONTENT = "content", ALL = "all";

    enum Type { TEXT, BOOL, INT, UUID, JSON, HASH }

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
            col("ROUTING_RULE_ID", Type.UUID), col("ATTRIBUTES", Type.JSON))));

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
        boolean hashes = false, selftest = false;
        for (int i = 0; i < args.length; i++) {
            switch (args[i]) {
                case "--database" -> database = value(args, ++i);
                case "--out" -> out = value(args, ++i);
                case "--set" -> set = value(args, ++i);
                case "--with-password-hashes" -> hashes = true;
                case "--selftest" -> selftest = true;
                default -> throw new Exit(USAGE, "unknown argument " + args[i]);
            }
        }
        if (selftest) {
            if (database != null || out != null) throw new Exit(USAGE, "--selftest takes no other argument");
            return selftest();
        }
        if (database == null || out == null) throw new Exit(USAGE, "--database and --out are required");
        if (!set.equals(CONTENT) && !set.equals(IDENTITY) && !set.equals(ALL)) {
            throw new Exit(USAGE, "--set is content, identity or all");
        }
        return export(database, Path.of(out), set, hashes);
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

    static int export(String base, Path out, String set, boolean hashes) throws Exception {
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
                for (Table t : tables) written.add(dump(c, t, out, hashes, created));
            } catch (SQLException e) {
                throw new Exit(FAILED, "reading failed: H2 error " + e.getErrorCode() + ", SQL state " + e.getSQLState());
            }
            if (!Stamp.of(file).equals(before)) {
                throw new Exit(FAILED, "the database file changed in size or time during the export");
            }
            writeManifest(out, set, driver, hashes, written, created);
            for (Written w : written) {
                System.out.println(w.table().file() + ": " + w.rows() + " rows, " + w.removed() + " fields removed");
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
        }
        throw new IllegalStateException("no reader for " + column.type());
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
                              List<Path> created) throws IOException {
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
        try (OutputStream stream = create(out.resolve("manifest.json"), created)) {
            stream.write((Json.write(manifest, true) + "\n").getBytes(StandardCharsets.UTF_8));
        }
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
