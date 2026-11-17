package main

import (
	"fmt"
	"reflect"

	"gopkg.in/yaml.v3"
)

func setServerArchived(target ServerConfig, archived bool) (*Config, error) {
	return editServersConfig(func(cfg *Config, servers *yaml.Node) error {
		index := target.sourceIndex
		var current *ServerConfig
		for i := range cfg.Servers {
			if cfg.Servers[i].sourceIndex == index {
				current = &cfg.Servers[i]
				break
			}
		}
		if current == nil || !reflect.DeepEqual(*current, target) || index < 0 || index >= len(servers.Content) {
			return fmt.Errorf("服务器配置已变化，请退出后重新打开")
		}
		entry := servers.Content[index]
		if entry.Kind == yaml.AliasNode || entry.Anchor != "" {
			// Override locally while leaving the shared anchor unchanged.
			entry = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Tag: "!!merge", Value: "<<"}, entry,
			}}
			servers.Content[index] = entry
		}
		if entry.Kind != yaml.MappingNode {
			return fmt.Errorf("服务器配置必须为 YAML 映射")
		}
		for i := 0; i < len(entry.Content); i += 2 {
			if entry.Content[i].Value == "archived" {
				value := *entry.Content[i+1]
				value.Kind, value.Tag, value.Value = yaml.ScalarNode, "!!bool", fmt.Sprint(archived)
				value.Style, value.Alias, value.Content, value.Anchor = 0, nil, nil, ""
				entry.Content[i+1] = &value
				return nil
			}
		}
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "archived"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(archived)},
		)
		return nil
	})
}
