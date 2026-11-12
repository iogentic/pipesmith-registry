package cli

// The help of each command, as argparse of Python 3.12 prints it at 80
// columns under the program name pipesmith-registry.
var commandHelp = map[string]string{
	"serve": "usage: pipesmith-registry serve [-h] [--port PORT] [--bind BIND]\n" +
		"                                [--server-names SERVER_NAMES]\n" +
		"                                [--ask-ldap-password]\n" +
		"\n" +
		"options:\n" +
		"  -h, --help            show this help message and exit\n" +
		"  --port PORT\n" +
		"  --bind BIND\n" +
		"  --server-names SERVER_NAMES\n" +
		"  --ask-ldap-password\n",
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
}
