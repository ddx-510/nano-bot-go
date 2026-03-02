package tools

import (
	"fmt"
	"log"
	"strings"
)

type Registry struct {
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

func (r *Registry) Register(t Tool) {
	r.tools[t.Name()] = t
	log.Printf("[tools] registered: %s", t.Name())
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) Schemas() []map[string]any {
	var schemas []map[string]any
	for _, t := range r.tools {
		schemas = append(schemas, Schema(t))
	}
	return schemas
}

const errorHint = "\n\n[Analyze the error above and try a different approach.]"

// Clone creates a shallow copy of the registry. Registered tools are shared
// references, so session-specific tools can be replaced without affecting the original.
func (r *Registry) Clone() *Registry {
	clone := &Registry{tools: make(map[string]Tool, len(r.tools))}
	for k, v := range r.tools {
		clone.tools[k] = v
	}
	return clone
}

func (r *Registry) Execute(name string, args map[string]any) string {
	t, ok := r.tools[name]
	if !ok {
		names := make([]string, 0, len(r.tools))
		for n := range r.tools {
			names = append(names, n)
		}
		return fmt.Sprintf("Error: tool '%s' not found. Available: %s", name, strings.Join(names, ", ")) + errorHint
	}
	result, err := t.Execute(args)
	if err != nil {
		return fmt.Sprintf("Error executing %s: %v", name, err) + errorHint
	}
	if strings.HasPrefix(result, "Error") {
		return result + errorHint
	}
	return result
}
