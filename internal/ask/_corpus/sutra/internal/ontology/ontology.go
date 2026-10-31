// Package ontology loads the YAML that says which entity types exist, which
// attributes they have, and how each source system's fields map onto them.
package ontology

import (
	"fmt"
	"os"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// AttrKind drives normalisation before matching.
type AttrKind string

const (
	String AttrKind = "string"
	Name   AttrKind = "name"
	GSTIN  AttrKind = "gstin"
	PAN    AttrKind = "pan"
	Phone  AttrKind = "phone"
	Email  AttrKind = "email"
	City   AttrKind = "city"
)

type Entity struct {
	Attributes map[string]AttrKind `yaml:"attributes"`
	// Description is shown to agents in the query tools.
	Description string `yaml:"description"`
}

type Source struct {
	Name   string            `yaml:"-"`
	Kind   string            `yaml:"kind"` // postgres_cdc | rest
	Entity string            `yaml:"entity"`
	Table  string            `yaml:"table,omitempty"` // postgres_cdc: schema.table
	Path   string            `yaml:"path,omitempty"`  // rest: resource path
	ID     string            `yaml:"id"`              // source field holding the record id
	Map    map[string]string `yaml:"map"`             // entity attribute -> source field
	// Priority decides survivorship in golden records: lower wins.
	Priority int `yaml:"priority"`
}

type Ontology struct {
	Entities map[string]Entity `yaml:"entities"`
	Sources  map[string]Source `yaml:"sources"`
}

func Load(path string) (*Ontology, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Ontology, error) {
	var o Ontology
	if err := yaml.Unmarshal(b, &o); err != nil {
		return nil, err
	}
	return &o, o.Validate()
}

func (o *Ontology) Validate() error {
	for name, s := range o.Sources {
		s.Name = name
		o.Sources[name] = s
		e, ok := o.Entities[s.Entity]
		if !ok {
			return fmt.Errorf("source %s: unknown entity %q", name, s.Entity)
		}
		if s.ID == "" {
			return fmt.Errorf("source %s: id field is required", name)
		}
		for attr := range s.Map {
			if _, ok := e.Attributes[attr]; !ok {
				return fmt.Errorf("source %s: entity %s has no attribute %q", name, s.Entity, attr)
			}
		}
		switch s.Kind {
		case "postgres_cdc":
			if s.Table == "" {
				return fmt.Errorf("source %s: table is required", name)
			}
		case "rest":
			if s.Path == "" {
				return fmt.Errorf("source %s: path is required", name)
			}
		default:
			return fmt.Errorf("source %s: unknown kind %q", name, s.Kind)
		}
	}
	return nil
}

// SourcesFor returns the sources feeding an entity type, by priority.
func (o *Ontology) SourcesFor(entity string) []Source {
	var out []Source
	for _, s := range o.Sources {
		if s.Entity == entity {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Record is one row from one source, already mapped to entity attributes.
type Record struct {
	Source    string            `json:"source"`
	ID        string            `json:"id"`
	Entity    string            `json:"entity"`
	Attrs     map[string]string `json:"attrs"`
	Deleted   bool              `json:"deleted,omitempty"`
	UpdatedAt time.Time         `json:"updated_at"` // source-side change time
	Position  string            `json:"position"`   // LSN or cursor that delivered it
}

func (r Record) Key() string { return r.Source + ":" + r.ID }

// MapRow turns raw source fields into a Record using the source mapping.
func (s Source) MapRow(row map[string]string) (Record, error) {
	id := row[s.ID]
	if id == "" {
		return Record{}, fmt.Errorf("source %s: row has no %q", s.Name, s.ID)
	}
	r := Record{Source: s.Name, ID: id, Entity: s.Entity, Attrs: map[string]string{}}
	for attr, field := range s.Map {
		if v, ok := row[field]; ok && v != "" {
			r.Attrs[attr] = v
		}
	}
	return r, nil
}
