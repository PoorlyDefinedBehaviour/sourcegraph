package conf

import (
	"encoding/json"

	"github.com/sourcegraph/sourcegraph/schema"
	"github.com/xeipuuv/gojsonschema"
)

// Validate validates the site configuration the JSON Schema and other custom validation
// checks.
func Validate(input string) (messages []string, err error) {
	normalizedInput := NormalizeJSON(input)

	res, err := validate([]byte(schema.SiteSchemaJSON), normalizedInput)
	if err != nil {
		return nil, err
	}
	messages = make([]string, len(res.Errors()))
	for i, e := range res.Errors() {
		messages[i] = e.String()
	}

	customMessages, err := validateCustom(normalizedInput)
	if err != nil {
		return nil, err
	}
	messages = append(messages, customMessages...)

	return messages, nil
}

func validate(schema, input []byte) (*gojsonschema.Result, error) {
	if len(input) > 0 {
		// HACK: Remove the "settings" field from site config because
		// github.com/xeipuuv/gojsonschema has a bug where $ref'd schemas do not always get
		// loaded. When https://github.com/xeipuuv/gojsonschema/pull/196 is merged, it will probably
		// be fixed. This means that the backend config validation will not validate settings, but
		// that is OK because specifying settings here is discouraged anyway.
		var v map[string]interface{}
		if err := json.Unmarshal(input, &v); err != nil {
			return nil, err
		}
		delete(v, "settings")
		var err error
		input, err = json.Marshal(v)
		if err != nil {
			return nil, err
		}
	}

	s, err := gojsonschema.NewSchema(jsonLoader{gojsonschema.NewBytesLoader(schema)})
	if err != nil {
		return nil, err
	}
	return s.Validate(gojsonschema.NewBytesLoader(input))
}

type jsonLoader struct {
	gojsonschema.JSONLoader
}

func (l jsonLoader) LoaderFactory() gojsonschema.JSONLoaderFactory {
	return &jsonLoaderFactory{}
}

type jsonLoaderFactory struct{}

func (f jsonLoaderFactory) New(source string) gojsonschema.JSONLoader {
	switch source {
	case "https://sourcegraph.com/v1/settings.schema.json":
		return gojsonschema.NewStringLoader(schema.SettingsSchemaJSON)
	case "https://sourcegraph.com/v1/site.schema.json":
		return gojsonschema.NewStringLoader(schema.SiteSchemaJSON)
	}
	return nil
}
