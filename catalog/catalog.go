// Package catalog describes the models overload knows about: where they come
// from, what their licences allow, how to run them and how they scored on
// overload's planted-bug eval. Administrators use it to restrict which models
// may review code (see Policy).
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Kinds of catalog entries.
const (
	KindOpen   = "open"
	KindHosted = "hosted"
)

// Commercial-use terms of a licence.
const (
	CommercialYes        = "yes"
	CommercialNo         = "no"
	CommercialRevenueCap = "revenue_cap"
	// CommercialProvider marks hosted models used under the provider's
	// service terms.
	CommercialProvider = "provider_terms"
	// CommercialUnknown means the catalog has not recorded the terms; an
	// administrator must review them.
	CommercialUnknown = ""
)

// Model is one catalog entry.
type Model struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Publisher string `json:"publisher"`
	// Origin is the ISO 3166 country code of the publisher's headquarters;
	// empty when not recorded.
	Origin       string `json:"origin"`
	Kind         string `json:"kind"`
	Architecture string `json:"architecture,omitempty"`
	// BaseModel names the model this one was built on; empty with
	// TrainedFromScratch for original models.
	BaseModel          string   `json:"base_model,omitempty"`
	TrainedFromScratch bool     `json:"trained_from_scratch,omitempty"`
	BaseOrigins        []string `json:"base_origins,omitempty"`
	// TrainingDataOrigins are the origins of models that generated part of
	// the training data.
	TrainingDataOrigins []string  `json:"training_data_origins,omitempty"`
	TrainingDataNotes   string    `json:"training_data_notes,omitempty"`
	Licence             Licence   `json:"licence"`
	ContextTokens       int       `json:"context_tokens,omitempty"`
	Weights             []Weights `json:"weights,omitempty"`
	Runtime             *Runtime  `json:"runtime,omitempty"`
	Settings            Settings  `json:"settings"`
	Evals               []Eval    `json:"evals,omitempty"`
	// Tier is the smallest hardware tier that runs the model well:
	// developer, team_node or team_cluster (see PLAN.md 16.2), or hosted.
	Tier    string   `json:"tier"`
	Notes   string   `json:"notes,omitempty"`
	Sources []string `json:"sources,omitempty"`
}

// Licence records what a model's licence allows.
type Licence struct {
	Name          string `json:"name,omitempty"`
	Commercial    string `json:"commercial"`
	RevenueCapUSD int64  `json:"revenue_cap_usd,omitempty"`
	Notes         string `json:"notes,omitempty"`
}

// Weights is one downloadable quantization.
type Weights struct {
	Quant      string  `json:"quant"`
	Repository string  `json:"repository,omitempty"`
	SizeGB     float64 `json:"size_gb,omitempty"`
	Format     string  `json:"format"`
}

// Runtime is how the model is served locally.
type Runtime struct {
	Engine string `json:"engine"`
	// MinBuild is the oldest llama.cpp build number known to load the
	// model; 0 when unknown or not llama.cpp.
	MinBuild   int      `json:"min_build,omitempty"`
	ServerArgs []string `json:"server_args,omitempty"`
	// ServedName is the model name to send in requests.
	ServedName string `json:"served_name,omitempty"`
}

// Settings are known-good overload model settings for the entry.
type Settings struct {
	ReasoningParam  string   `json:"reasoning_param,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
}

// Eval is a measured result on the planted-bug eval (deploy/README.md):
// Found of Possible planted bugs over Runs runs.
type Eval struct {
	Setup    string `json:"setup"`
	Found    int    `json:"found"`
	Possible int    `json:"possible"`
	Runs     int    `json:"runs"`
	Notes    string `json:"notes,omitempty"`
}

// Score formats an eval result such as "15 of 20".
func (eval Eval) Score() string {
	return fmt.Sprintf("%d of %d", eval.Found, eval.Possible)
}

// BestEval returns the entry's highest-scoring eval, if any.
func (model Model) BestEval() (Eval, bool) {
	if len(model.Evals) == 0 {
		return Eval{}, false
	}
	best := model.Evals[0]
	for _, eval := range model.Evals[1:] {
		if eval.Found*best.Possible > best.Found*eval.Possible {
			best = eval
		}
	}
	return best, true
}

// Lineage lists the model's own origin followed by the origins of its base
// model and of models that generated its training data, without repeats.
func (model Model) Lineage() []string {
	var origins []string
	for _, origin := range append(append([]string{model.Origin}, model.BaseOrigins...), model.TrainingDataOrigins...) {
		if !slices.Contains(origins, origin) {
			origins = append(origins, origin)
		}
	}
	return origins
}

//go:embed models.json
var modelsJSON []byte

var models = mustLoad(modelsJSON)

func mustLoad(data []byte) []Model {
	loaded, err := parse(data)
	if err != nil {
		panic("catalog: " + err.Error())
	}
	return loaded
}

var (
	idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)
	// reservedIDs are catalog page paths an entry cannot use.
	reservedIDs   = []string{"policy"}
	originPattern = regexp.MustCompile(`^[A-Z]{2}$`)
	tiers         = []string{"developer", "team_node", "team_cluster", "hosted"}
	commercial    = []string{CommercialYes, CommercialNo, CommercialRevenueCap, CommercialProvider, CommercialUnknown}
)

func parse(data []byte) ([]Model, error) {
	var loaded []Model
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&loaded); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, model := range loaded {
		if err := model.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", model.ID, err)
		}
		if seen[model.ID] {
			return nil, fmt.Errorf("%s: duplicate id", model.ID)
		}
		seen[model.ID] = true
	}
	return loaded, nil
}

func (model Model) validate() error {
	switch {
	case !idPattern.MatchString(model.ID) || slices.Contains(reservedIDs, model.ID):
		return fmt.Errorf("invalid id")
	case model.Name == "" || model.Publisher == "":
		return fmt.Errorf("name and publisher are required")
	case model.Origin != "" && !originPattern.MatchString(model.Origin):
		return fmt.Errorf("origin must be an ISO country code")
	case model.Kind != KindOpen && model.Kind != KindHosted:
		return fmt.Errorf("kind must be open or hosted")
	case !slices.Contains(tiers, model.Tier):
		return fmt.Errorf("unknown tier %q", model.Tier)
	case !slices.Contains(commercial, model.Licence.Commercial):
		return fmt.Errorf("unknown commercial terms %q", model.Licence.Commercial)
	case (model.Licence.Commercial == CommercialRevenueCap) != (model.Licence.RevenueCapUSD > 0):
		return fmt.Errorf("a revenue cap needs revenue_cap_usd and the other way round")
	case model.Kind == KindHosted && (model.Runtime != nil || len(model.Weights) > 0):
		return fmt.Errorf("hosted models have no weights or runtime")
	case model.TrainedFromScratch && model.BaseModel != "":
		return fmt.Errorf("a model trained from scratch has no base model")
	}
	for _, origin := range append(slices.Clone(model.BaseOrigins), model.TrainingDataOrigins...) {
		if !originPattern.MatchString(origin) {
			return fmt.Errorf("lineage origins must be ISO country codes")
		}
	}
	for _, eval := range model.Evals {
		if eval.Setup == "" || eval.Runs < 1 || eval.Possible < 1 || eval.Found < 0 || eval.Found > eval.Possible {
			return fmt.Errorf("invalid eval %q", eval.Setup)
		}
	}
	for _, weights := range model.Weights {
		if weights.Quant == "" || weights.Format == "" || weights.SizeGB < 0 {
			return fmt.Errorf("invalid weights")
		}
	}
	return nil
}

// Models returns a copy of every catalog entry in catalog order.
func Models() []Model {
	out := make([]Model, len(models))
	for index, model := range models {
		out[index] = model.clone()
	}
	return out
}

// Lookup returns a copy of the entry with id.
func Lookup(id string) (Model, bool) {
	for _, model := range models {
		if model.ID == id {
			return model.clone(), true
		}
	}
	return Model{}, false
}

// Identify returns the catalog entry a model connection refers to: the
// entry it names, or else the entry whose ID or served model name equals
// the model ID it sends. It fails when the named entry does not exist, or
// when the model ID is another entry's, so a connection cannot be labelled
// as a different catalog model.
func Identify(catalogID, modelName string) (Model, bool, error) {
	var byName *Model
	for index := range models {
		entry := &models[index]
		if strings.EqualFold(modelName, entry.ID) || (entry.Runtime != nil && entry.Runtime.ServedName != "" && strings.EqualFold(modelName, entry.Runtime.ServedName)) {
			byName = entry
			break
		}
	}
	if catalogID == "" {
		if byName == nil {
			return Model{}, false, nil
		}
		return byName.clone(), true, nil
	}
	named, ok := Lookup(catalogID)
	if !ok {
		return Model{}, false, fmt.Errorf("catalog entry %q does not exist", catalogID)
	}
	if byName != nil && byName.ID != named.ID {
		return Model{}, false, fmt.Errorf("model %q is catalog entry %s, not %s", modelName, byName.ID, named.ID)
	}
	return named, true, nil
}

// clone deep-copies the entry so callers cannot change the shared catalog.
func (model Model) clone() Model {
	model.BaseOrigins = slices.Clone(model.BaseOrigins)
	model.TrainingDataOrigins = slices.Clone(model.TrainingDataOrigins)
	model.Weights = slices.Clone(model.Weights)
	model.Evals = slices.Clone(model.Evals)
	model.Sources = slices.Clone(model.Sources)
	if model.Runtime != nil {
		runtime := *model.Runtime
		runtime.ServerArgs = slices.Clone(runtime.ServerArgs)
		model.Runtime = &runtime
	}
	if model.Settings.Temperature != nil {
		temperature := *model.Settings.Temperature
		model.Settings.Temperature = &temperature
	}
	return model
}
