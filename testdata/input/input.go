package main

import (
	"context"
	"errors"
	"strings"
	"text/template"
	"time"

	"github.com/calyptia/plugin"
	"github.com/calyptia/plugin/metric"
)

func init() {
	plugin.RegisterInput("go-test-input-plugin", "Golang input plugin for testing", &inputPlugin{})
}

type inputPlugin struct {
	foo            string
	tmpl           *template.Template
	multilineSplit []string
	collectCounter metric.Counter
	log            plugin.Logger
}

func (plug *inputPlugin) Init(ctx context.Context, fbit *plugin.Fluentbit) error {
	var err error
	plug.tmpl, err = template.New("test").Parse(fbit.Conf.String("tmpl"))
	if err != nil {
		return err
	}

	plug.foo = fbit.Conf.String("foo")
	plug.multilineSplit = strings.Split(fbit.Conf.String("multiline"), "\n")
	plug.collectCounter = fbit.Metrics.NewCounter("collect_total", "Total number of collects", "go-test-input-plugin")
	plug.log = fbit.Logger
	return nil
}

func (plug inputPlugin) Collect(ctx context.Context, send plugin.SendFunc) error {
	tick := time.NewTicker(time.Second)

	for {
		select {
		case <-ctx.Done():
			err := ctx.Err()
			if err != nil && !errors.Is(err, context.Canceled) {
				plug.log.Error("[go-test-input-plugin] operation failed")
				return err
			}

			return nil
		case <-tick.C:
			var buff strings.Builder
			err := plug.tmpl.Execute(&buff, nil)
			if err != nil {
				return err
			}

			plug.collectCounter.Add(1)

			start := time.Now()
			for i := 0; i < 10; i++ {
				plug.log.Debug("[go-test-input-plugin] sending skip_me message")
				send(time.Now().UTC(), map[string]any{
					"skip_me": true,
				})
			}
			took := time.Since(start)

			plug.log.Debug("[go-test-input-plugin] sending many skip_me messages took %s", took)

			plug.log.Debug("[go-test-input-plugin] sending message")
			send(time.Now().UTC(), map[string]any{
				"message":         "hello from go-test-input-plugin",
				"foo":             plug.foo,
				"tmpl_out":        buff.String(),
				"multiline_split": plug.multilineSplit,
				"took":            took.String(),
			})
		}
	}
}

func main() {}
