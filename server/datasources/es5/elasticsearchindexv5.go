package es5

import (
	"context"
	"encoding/json"
	"io"

	logging "github.com/op/go-logging"
	cache "github.com/patrickmn/go-cache"

	"net/url"
	"path"

	elastic "gopkg.in/olivere/elastic.v5"

	"fmt"

	"github.com/dutchcoders/marija/server/datasources"
)

// implement via
// return partial results to websocket
// scripted fields from config

var (
	_ = datasources.Register("elasticsearch", New)
)

var log = logging.MustGetLogger("marija/datasources/elasticsearch")

func New(options ...func(datasources.Index) error) (datasources.Index, error) {
	s := Elasticsearch{}

	for _, optionFn := range options {
		optionFn(&s)
	}

	params := []elastic.ClientOptionFunc{
		elastic.SetURL(s.URL.String()),
		elastic.SetSniff(false),
	}

	if s.Username != "" {
		params = append(params, elastic.SetBasicAuth(s.Username, s.Password))
	}

	if client, err := elastic.NewClient(
		params...,
	); err != nil {
		return nil, fmt.Errorf("Error connecting to: %s: %s", s.URL.String(), err)
	} else {
		s.client = client
	}

	return &s, nil
}

func (m *Elasticsearch) Type() string {
	return "elasticsearch"
}

type Config struct {
	URL url.URL

	Index string

	Username string
	Password string
}

type Elasticsearch struct {
	client *elastic.Client
	cache  *cache.Cache

	Config
}

func (m *Elasticsearch) UnmarshalTOML(p interface{}) error {
	data, _ := p.(map[string]interface{})

	if v, ok := data["username"]; !ok {
	} else if v, ok := v.(string); !ok {
	} else {
		m.Username = v
	}

	if v, ok := data["password"]; !ok {
	} else if v, ok := v.(string); !ok {
	} else {
		m.Password = v
	}

	if v, ok := data["url"]; !ok {
	} else if v, ok := v.(string); !ok {
	} else if u, err := url.Parse(v); err != nil {
		return err
	} else {
		m.Index = path.Base(u.Path)

		u.Path = ""
		m.URL = *u
	}

	return nil
}

func (i *Elasticsearch) Search(ctx context.Context, so datasources.SearchOptions) datasources.SearchResponse {
	itemCh := make(chan datasources.Item)
	errorCh := make(chan error)

	go func() {
		defer close(itemCh)
		defer close(errorCh)

		hl := elastic.NewHighlight()
		hl = hl.Fields(elastic.NewHighlighterField("*").RequireFieldMatch(false).NumOfFragments(0))
		hl = hl.PreTags("<em>").PostTags("</em>")

		scriptFields := []*elastic.ScriptField{
		/*
			//		elastic.NewScriptField("src-ip_dst-ip_port", elastic.NewScript("params['_source']['source-ip'] + '_' + params['_source']['destination-ip'] + '_' + params['_source']['destination-port']")),
			elastic.NewScriptField("src-ip_dst-net_port", elastic.NewScript("params['_source']['source-ip'] + '_' + params['_source']['destination-net'] + '_' + params['_source']['destination-port']")),
		*/
		}

		q := elastic.NewQueryStringQuery(so.Query)

		src := elastic.NewSearchSource().
			Query(q).
			FetchSource(false).
			Highlight(hl).
			FetchSource(true).
			From(so.From).
			Size(100)

		if len(scriptFields) > 0 {
			src = src.ScriptFields(scriptFields...)
		}

		hits := make(chan *elastic.SearchHit)

		go func() {
			defer close(hits)

			scroll := i.client.Scroll().Index(i.Index).SearchSource(src)
			for {
				results, err := scroll.Do(ctx)
				if err == io.EOF {
					return
				} else if err != nil {
					errorCh <- err
					return
				}

				for _, hit := range results.Hits.Hits {
					select {
					case hits <- hit:
					case <-ctx.Done():
						log.Debug("Search canceled query=%s, index=%s", q, i.Index)
						return
					}
				}
			}

		}()

		for {
			select {
			case hit, ok := <-hits:
				if !ok {
					return
				}

				var fields map[string]interface{}
				if err := json.Unmarshal(*hit.Source, &fields); err != nil {
					errorCh <- err
					continue
				}

				fields = flattenFields("", fields)

				for key, val := range hit.Fields {
					fields[key] = val
				}

				itemCh <- datasources.Item{
					ID:        hit.Id,
					Fields:    fields,
					Highlight: hit.Highlight,
				}
			}
		}

		return
	}()

	return datasources.NewSearchResponse(
		itemCh,
		errorCh,
	)
}

func flattenFields(root string, m map[string]interface{}) map[string]interface{} {
	fields := map[string]interface{}{}

	for k, v := range m {
		key := k
		if root != "" {
			key = root + "." + key
		}

		switch s2 := v.(type) {
		case map[string]interface{}:
			for k2, v2 := range flattenFields(key, s2) {
				fields[k2] = v2
			}
		default:
			fields[key] = v
		}
	}

	return fields
}

func flatten(root string, m map[string]interface{}) (fields []datasources.Field) {
	for k, v := range m {
		if k == "mappings" {
			fields = append(fields, flatten(root, v.(map[string]interface{}))...)
		} else if k == "properties" {
			fields = append(fields, flatten(root, v.(map[string]interface{}))...)
		} else if k == "fields" {
			fields = append(fields, flatten(root, v.(map[string]interface{}))...)
		} else {
			if v2, ok := v.(map[string]interface{}); ok {
				key := k
				if root != "" {
					key = root + "." + key
				}
				fields = append(fields, flatten(key, v2)...)
			} else if k == "type" {
				fields = append(fields, datasources.Field{
					Path: root,
					Type: v.(string),
				})
			}
		}
	}

	return
}

func (i *Elasticsearch) GetFields(ctx context.Context) (fields []datasources.Field, err error) {
	exists, err := i.client.IndexExists(i.Index).Do(ctx)
	if err != nil {
		return nil, err
	} else if !exists {
		return nil, fmt.Errorf("Index %s doesn't exist", i.Index)
	}

	mapping, err := i.client.GetMapping().
		Index(i.Index).
		Do(ctx)

	if err != nil {
		return nil, fmt.Errorf("Error retrieving fields for index: %s: %s", i.Index, err.Error())
	}

	mapping = mapping[i.Index].(map[string]interface{})
	mapping = mapping["mappings"].(map[string]interface{})
	for _, v := range mapping {
		fields = append(fields, flatten("", v.(map[string]interface{}))...)
	}

	/*
		fields = append(fields, datasources.Field{
			Path: "src-ip_dst-net_port",
			Type: "string",
		})

		fields = append(fields, datasources.Field{
			Path: "src-ip_dst-ip_port",
			Type: "string",
		})
	*/

	return
}
