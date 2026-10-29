package main

import (
	"log"

	"github.com/justmike1/arbetern/config"
	"github.com/justmike1/arbetern/internal/kube"
	"github.com/justmike1/arbetern/projects"
)

func projectsCluster(cfg *config.Config) projects.ClusterConfig {
	kc, err := kube.InCluster(kube.ServiceAccountDir, projects.UserAgent)
	if err != nil {
		log.Fatalf("projects: session Jobs need arbetern's service account token: %v", err)
	}
	return projects.ClusterConfig{
		Kube:       kc,
		Namespace:  cfg.ProjectsNamespace,
		Template:   cfg.ProjectsJobTemplate,
		GatewayURL: cfg.ProjectsGatewayURL,
		APIKey:     cfg.AnthropicAPIKey,
		Model:      cfg.ProjectsModel,
	}
}
