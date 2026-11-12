package authz

import "github.com/iogentic/pipesmith-registry/internal/model"

// The built in privileges and roles of the old product, from the security
// contributors of the public source of the old product, tag release-3.70.1-02
// (the base security contributor, SecurityContributorSupport,
// RepositoryViewSecurityContributor, RepositoryFormatSecurityContributor,
// RepositoryAdminSecurityContributor,
// ScriptSecurityContributor and the contributors of logging, metrics,
// capabilities, tasks, selectors, blob stores, data stores, analytics,
// support tools, wonderland and the trust store), read 2026-11-12.

// Action lists of SecurityContributorSupport: create, update and delete
// bring read along.
const (
	actionCreate = "create,read"
	actionUpdate = "update,read"
	actionDelete = "delete,read"
)

func application(id, domain, actions string) model.Privilege {
	return model.Privilege{Name: id, Type: KindApplication, ReadOnly: true,
		Properties: map[string]string{"domain": domain, "actions": actions}}
}

// crud is createCrudAndAllApplicationPrivileges: all, create, read, update
// and delete of one domain.
func crud(prefix, domain string) []model.Privilege {
	return []model.Privilege{
		application(prefix+"-all", domain, all),
		application(prefix+"-create", domain, actionCreate),
		application(prefix+"-read", domain, "read"),
		application(prefix+"-update", domain, actionUpdate),
		application(prefix+"-delete", domain, actionDelete),
	}
}

func repository(kind, format, name, action string) model.Privilege {
	return model.Privilege{Name: "nx-" + kind + "-" + format + "-" + name + "-" + action, Type: kind, ReadOnly: true,
		Properties: map[string]string{"format": format, "repository": name, "actions": action}}
}

var breadActions = []string{all, "browse", "read", "edit", "add", "delete"}

// builtinPrivileges are the privileges that exist without a stored row and
// do not depend on the repositories.
func builtinPrivileges() []model.Privilege {
	list := []model.Privilege{
		{Name: "nx-all", Type: KindWildcard, ReadOnly: true, Properties: map[string]string{"pattern": "nexus:*"}},
		application("nx-settings-all", "settings", all),
		application("nx-settings-read", "settings", "read"),
		application("nx-settings-update", "settings", actionUpdate),
		application("nx-bundles-all", "bundles", all),
		application("nx-bundles-read", "bundles", "read"),
		application("nx-search-read", "search", "read"),
		application("nx-apikey-all", "apikey", all),
		application("nx-userschangepw", "userschangepw", actionCreate),
		application("nx-component-upload", "component", "create"),
		application("nx-logging-all", "logging", all),
		application("nx-logging-read", "logging", "read"),
		application("nx-logging-update", "logging", actionUpdate),
		application("nx-logging-mark", "logging", "create"),
		application("nx-metrics-all", "metrics", all),
		application("nx-analytics-all", "analytics", all),
		application("nx-atlas-all", "atlas", all),
		application("nx-wonderland-all", "wonderland", all),
		application("nx-tasks-run", "tasks", "start,stop"),
	}
	for _, c := range [][2]string{
		{"nx-privileges", "privileges"}, {"nx-roles", "roles"}, {"nx-users", "users"},
		{"nx-capabilities", "capabilities"}, {"nx-tasks", "tasks"}, {"nx-selectors", "selectors"},
		{"nx-blobstores", "blobstores"}, {"nx-datastores", "datastores"}, {"nx-ssl-truststore", "ssl-truststore"},
	} {
		list = append(list, crud(c[0], c[1])...)
	}
	for _, a := range breadActions {
		list = append(list, repository(KindRepositoryView, all, all, a), repository(KindRepositoryAdmin, all, all, a))
	}
	for _, a := range []string{all, "browse", "read", "edit", "add", "delete", "run"} {
		actions := a
		if a != all && a != "read" && a != "run" {
			actions = a + ",read"
		}
		list = append(list, model.Privilege{Name: "nx-script-*-" + a, Type: KindScript, ReadOnly: true,
			Properties: map[string]string{"name": all, "actions": actions}})
	}
	return list
}

// generatedPrivileges follow the repositories: the view privileges of each
// format and of each repository, and the administration privileges of each
// repository, which have no add action.
func generatedPrivileges(repositories []model.Repository) []model.Privilege {
	var list []model.Privilege
	formats := map[string]bool{}
	for _, r := range repositories {
		if !formats[r.Format] {
			formats[r.Format] = true
			for _, a := range breadActions {
				list = append(list, repository(KindRepositoryView, r.Format, all, a))
			}
		}
		for _, a := range breadActions {
			list = append(list, repository(KindRepositoryView, r.Format, r.Name, a))
			if a != "add" {
				list = append(list, repository(KindRepositoryAdmin, r.Format, r.Name, a))
			}
		}
	}
	return list
}

// builtinRoles are nx-admin and nx-anonymous.
func builtinRoles() []model.Role {
	return []model.Role{
		{ID: "nx-admin", Name: "nx-admin", Description: "Administrator Role", ReadOnly: true, Source: "default",
			Privileges: []string{"nx-all"}},
		{ID: "nx-anonymous", Name: "nx-anonymous", Description: "Anonymous Role", ReadOnly: true, Source: "default",
			Privileges: []string{"nx-search-read", "nx-healthcheck-read", "nx-repository-view-*-*-browse",
				"nx-repository-view-*-*-read"}},
	}
}
