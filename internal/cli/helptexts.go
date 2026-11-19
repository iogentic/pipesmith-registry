package cli

// The help of each command, as argparse of Python 3.12 prints it at 80
// columns under the program name pipesmith-registry.
var commandHelp = map[string]string{
	"serve": "usage: pipesmith-registry serve [-h] [--port PORT] [--bind BIND]\n" +
		"                                [--server-names SERVER_NAMES]\n" +
		"                                [--ask-ldap-password] [--tls-cert FILE]\n" +
		"                                [--tls-key FILE] [--writes]\n" +
		"\n" +
		"options:\n" +
		"  -h, --help            show this help message and exit\n" +
		"  --port PORT\n" +
		"  --bind BIND\n" +
		"  --server-names SERVER_NAMES\n" +
		"  --ask-ldap-password\n" +
		"  --tls-cert FILE\n" +
		"  --tls-key FILE\n" +
		"  --writes\n",
	"store": "usage: pipesmith-registry store [-h] {add,list,remove} ...\n" +
		"\n" +
		"positional arguments:\n" +
		"  {add,list,remove}\n" +
		"\n" +
		"options:\n" +
		"  -h, --help         show this help message and exit\n",
	"store add": "usage: pipesmith-registry store add [-h] --kind {nexus-file,directory} --root\n" +
		"                                    ROOT [--repos REPOS] [--manifest MANIFEST]\n" +
		"                                    name\n" +
		"\n" +
		"positional arguments:\n" +
		"  name\n" +
		"\n" +
		"options:\n" +
		"  -h, --help            show this help message and exit\n" +
		"  --kind {nexus-file,directory}\n" +
		"  --root ROOT\n" +
		"  --repos REPOS\n" +
		"  --manifest MANIFEST\n",
	"store list": "usage: pipesmith-registry store list [-h]\n" +
		"\n" +
		"options:\n" +
		"  -h, --help  show this help message and exit\n",
	"store remove": "usage: pipesmith-registry store remove [-h] name\n" +
		"\n" +
		"positional arguments:\n" +
		"  name\n" +
		"\n" +
		"options:\n" +
		"  -h, --help  show this help message and exit\n",
	"scan": "usage: pipesmith-registry scan [-h] [name]\n" +
		"\n" +
		"positional arguments:\n" +
		"  name\n" +
		"\n" +
		"options:\n" +
		"  -h, --help  show this help message and exit\n",
	"repos": "usage: pipesmith-registry repos [-h] [--json] {port} ...\n" +
		"\n" +
		"positional arguments:\n" +
		"  {port}\n" +
		"\n" +
		"options:\n" +
		"  -h, --help  show this help message and exit\n" +
		"  --json\n",
	"repos port": "usage: pipesmith-registry repos port [-h] name <port|none>\n" +
		"\n" +
		"positional arguments:\n" +
		"  name\n" +
		"  <port|none>\n" +
		"\n" +
		"options:\n" +
		"  -h, --help   show this help message and exit\n",
	"docker-check": "usage: pipesmith-registry docker-check [-h] name\n" +
		"\n" +
		"positional arguments:\n" +
		"  name\n" +
		"\n" +
		"options:\n" +
		"  -h, --help  show this help message and exit\n",
	"import-settings": "usage: pipesmith-registry import-settings [-h] file\n" +
		"\n" +
		"positional arguments:\n" +
		"  file\n" +
		"\n" +
		"options:\n" +
		"  -h, --help  show this help message and exit\n",
	"import": "usage: pipesmith-registry import [-h] [--data DATA] directory\n" +
		"\n" +
		"positional arguments:\n" +
		"  directory\n" +
		"\n" +
		"options:\n" +
		"  -h, --help   show this help message and exit\n" +
		"  --data DATA\n",
	"import-state": "usage: pipesmith-registry import-state [-h] --settings SETTINGS [--data DATA]\n" +
		"\n" +
		"options:\n" +
		"  -h, --help           show this help message and exit\n" +
		"  --settings SETTINGS\n" +
		"  --data DATA\n",
	"state": "usage: pipesmith-registry state [-h] {show} ...\n" +
		"\n" +
		"positional arguments:\n" +
		"  {show}\n" +
		"\n" +
		"options:\n" +
		"  -h, --help  show this help message and exit\n",
	"state show": "usage: pipesmith-registry state show [-h] [--data DATA]\n" +
		"\n" +
		"options:\n" +
		"  -h, --help   show this help message and exit\n" +
		"  --data DATA\n",
	"user": "usage: pipesmith-registry user [-h] {add,password,enable,disable,roles} ...\n" +
		"\n" +
		"positional arguments:\n" +
		"  {add,password,enable,disable,roles}\n" +
		"\n" +
		"options:\n" +
		"  -h, --help            show this help message and exit\n",
	"user add": "usage: pipesmith-registry user add [-h] [--roles ROLES]\n" +
		"                                   [--first-name FIRST_NAME]\n" +
		"                                   [--last-name LAST_NAME] [--email EMAIL]\n" +
		"                                   [--data DATA]\n" +
		"                                   name\n" +
		"\n" +
		"positional arguments:\n" +
		"  name\n" +
		"\n" +
		"options:\n" +
		"  -h, --help            show this help message and exit\n" +
		"  --roles ROLES\n" +
		"  --first-name FIRST_NAME\n" +
		"  --last-name LAST_NAME\n" +
		"  --email EMAIL\n" +
		"  --data DATA\n",
	"user password": nameHelp("user password"),
	"user enable":   nameHelp("user enable"),
	"user disable":  nameHelp("user disable"),
	"user roles": "usage: pipesmith-registry user roles [-h] [--data DATA] name [roles]\n" +
		"\n" +
		"positional arguments:\n" +
		"  name\n" +
		"  roles\n" +
		"\n" +
		"options:\n" +
		"  -h, --help   show this help message and exit\n" +
		"  --data DATA\n",
	"anonymous": "usage: pipesmith-registry anonymous [-h] {on,off} ...\n" +
		"\n" +
		"positional arguments:\n" +
		"  {on,off}\n" +
		"\n" +
		"options:\n" +
		"  -h, --help  show this help message and exit\n",
	"anonymous on": "usage: pipesmith-registry anonymous on [-h] [--user USER] [--roles ROLES]\n" +
		"                                       [--data DATA]\n" +
		"\n" +
		"options:\n" +
		"  -h, --help     show this help message and exit\n" +
		"  --user USER\n" +
		"  --roles ROLES\n" +
		"  --data DATA\n",
	"anonymous off": dataHelp("anonymous off"),
	"realms": "usage: pipesmith-registry realms [-h] {list,set} ...\n" +
		"\n" +
		"positional arguments:\n" +
		"  {list,set}\n" +
		"\n" +
		"options:\n" +
		"  -h, --help  show this help message and exit\n",
	"realms list": dataHelp("realms list"),
	"realms set": "usage: pipesmith-registry realms set [-h] [--default-role DEFAULT_ROLE]\n" +
		"                                     [--data DATA]\n" +
		"                                     realms\n" +
		"\n" +
		"positional arguments:\n" +
		"  realms\n" +
		"\n" +
		"options:\n" +
		"  -h, --help            show this help message and exit\n" +
		"  --default-role DEFAULT_ROLE\n" +
		"  --data DATA\n",
	"ldap": "usage: pipesmith-registry ldap [-h] {password,ca,check} ...\n" +
		"\n" +
		"positional arguments:\n" +
		"  {password,ca,check}\n" +
		"\n" +
		"options:\n" +
		"  -h, --help           show this help message and exit\n",
	"ldap ca": "usage: pipesmith-registry ldap ca [-h] [--data DATA] connection <file|none>\n" +
		"\n" +
		"positional arguments:\n" +
		"  connection\n" +
		"  <file|none>\n" +
		"\n" +
		"options:\n" +
		"  -h, --help   show this help message and exit\n" +
		"  --data DATA\n",
	"ldap password": "usage: pipesmith-registry ldap password [-h] [--data DATA] connection\n" +
		"\n" +
		"positional arguments:\n" +
		"  connection\n" +
		"\n" +
		"options:\n" +
		"  -h, --help   show this help message and exit\n" +
		"  --data DATA\n",
	"ldap check": "usage: pipesmith-registry ldap check [-h] [--data DATA] [connection]\n" +
		"\n" +
		"positional arguments:\n" +
		"  connection\n" +
		"\n" +
		"options:\n" +
		"  -h, --help   show this help message and exit\n" +
		"  --data DATA\n",
	"token": "usage: pipesmith-registry token [-h] {issue,list,revoke} ...\n" +
		"\n" +
		"positional arguments:\n" +
		"  {issue,list,revoke}\n" +
		"\n" +
		"options:\n" +
		"  -h, --help           show this help message and exit\n",
	"token issue": "usage: pipesmith-registry token issue [-h] --kind {npm,user}\n" +
		"                                      [--source {default,LDAP}] [--data DATA]\n" +
		"                                      user\n" +
		"\n" +
		"positional arguments:\n" +
		"  user\n" +
		"\n" +
		"options:\n" +
		"  -h, --help            show this help message and exit\n" +
		"  --kind {npm,user}\n" +
		"  --source {default,LDAP}\n" +
		"  --data DATA\n",
	"token list": "usage: pipesmith-registry token list [-h] [--source {default,LDAP}]\n" +
		"                                     [--data DATA]\n" +
		"                                     user\n" +
		"\n" +
		"positional arguments:\n" +
		"  user\n" +
		"\n" +
		"options:\n" +
		"  -h, --help            show this help message and exit\n" +
		"  --source {default,LDAP}\n" +
		"  --data DATA\n",
	"token revoke": "usage: pipesmith-registry token revoke [-h] [--data DATA] id\n" +
		"\n" +
		"positional arguments:\n" +
		"  id\n" +
		"\n" +
		"options:\n" +
		"  -h, --help   show this help message and exit\n" +
		"  --data DATA\n",
	"maven": "usage: pipesmith-registry maven [-h] {rebuild-metadata} ...\n" +
		"\n" +
		"positional arguments:\n" +
		"  {rebuild-metadata}\n" +
		"\n" +
		"options:\n" +
		"  -h, --help          show this help message and exit\n",
	"maven rebuild-metadata": "usage: pipesmith-registry maven rebuild-metadata [-h] [--group GROUP]\n" +
		"                                                 [--artifact ARTIFACT]\n" +
		"                                                 [--version VERSION]\n" +
		"                                                 repository\n" +
		"\n" +
		"positional arguments:\n" +
		"  repository\n" +
		"\n" +
		"options:\n" +
		"  -h, --help           show this help message and exit\n" +
		"  --group GROUP\n" +
		"  --artifact ARTIFACT\n" +
		"  --version VERSION\n",
	"config": "usage: pipesmith-registry config [-h]\n" +
		"                                 [--get KEY | --check | --json | --docker-ports]\n" +
		"\n" +
		"options:\n" +
		"  -h, --help      show this help message and exit\n" +
		"  --get KEY\n" +
		"  --check\n" +
		"  --json\n" +
		"  --docker-ports\n",
	"version": "usage: pipesmith-registry version [-h]\n" +
		"\n" +
		"options:\n" +
		"  -h, --help  show this help message and exit\n",
	"update": "usage: pipesmith-registry update [-h] [--root ROOT] [--allow-downgrade]\n" +
		"                                 [--fetch] [--version VERSION]\n" +
		"                                 [--switch VERSION] [--no-catalog-backup]\n" +
		"                                 [--scheduled]\n" +
		"                                 [directory]\n" +
		"\n" +
		"positional arguments:\n" +
		"  directory\n" +
		"\n" +
		"options:\n" +
		"  -h, --help           show this help message and exit\n" +
		"  --root ROOT\n" +
		"  --allow-downgrade\n" +
		"  --fetch\n" +
		"  --version VERSION\n" +
		"  --switch VERSION\n" +
		"  --no-catalog-backup\n" +
		"  --scheduled\n",
	"schema": "usage: pipesmith-registry schema [-h]\n" +
		"\n" +
		"options:\n" +
		"  -h, --help  show this help message and exit\n",
	"rollback": "usage: pipesmith-registry rollback [-h] [--root ROOT]\n" +
		"\n" +
		"options:\n" +
		"  -h, --help   show this help message and exit\n" +
		"  --root ROOT\n",
}

// nameHelp is the help of a command with the positional name and --data,
// whose usage fits one line.
func nameHelp(command string) string {
	return "usage: pipesmith-registry " + command + " [-h] [--data DATA] name\n" +
		"\n" +
		"positional arguments:\n" +
		"  name\n" +
		"\n" +
		"options:\n" +
		"  -h, --help   show this help message and exit\n" +
		"  --data DATA\n"
}

// dataHelp is the help of a command with --data alone.
func dataHelp(command string) string {
	return "usage: pipesmith-registry " + command + " [-h] [--data DATA]\n" +
		"\n" +
		"options:\n" +
		"  -h, --help   show this help message and exit\n" +
		"  --data DATA\n"
}
