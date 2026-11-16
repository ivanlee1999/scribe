// transcription-e2e generates public synthetic ink and explicitly targets disposable test databases.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	action := flag.String("action", "generate", "generate, seed, apply, metadata, snapshot, wait or verify")
	out := flag.String("out", "/tmp/notable-llm-e2e-fixtures", "fixture artifact directory")
	phaseName := flag.String("phase", "01-milk", "manifest phase name")
	isolated := flag.Bool("confirm-isolated", false, "allow requests only to loopback llm_e2e_* databases")
	timeout := flag.Duration("timeout", 5*time.Minute, "maximum wait for real provider result")
	layout := flag.Bool("layout", false, "use the synthetic heading/list/task/table fixture")
	flag.Parse()
	m := fixtures()
	if *layout {
		m = layoutFixtures()
	}
	if *action == "generate" {
		if err := writeFixtures(*out, m); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(*out + "/manifest.json")
		return
	}
	driver, err := newDriver(*isolated)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var selected *phase
	for index := range m.Phases {
		if m.Phases[index].Name == *phaseName {
			selected = &m.Phases[index]
		}
	}
	if selected == nil {
		fmt.Fprintln(os.Stderr, "unknown fixture phase")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var report document
	switch *action {
	case "seed":
		err = driver.seed(ctx, m)
	case "apply":
		err = driver.apply(ctx, m, *selected)
	case "snapshot":
		report, err = driver.snapshot(ctx, m, *selected)
	case "metadata":
		err = driver.metadata(ctx, m, *selected)
	case "wait":
		report, err = driver.wait(ctx, m, *selected)
	case "verify":
		report, err = driver.verify(ctx, m, *selected)
	default:
		err = fmt.Errorf("unknown action %q", *action)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if report != nil {
		kind := "verified"
		if *action == "snapshot" {
			kind = "source-before"
		}
		if err = saveReport(*out, *selected, report, kind); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(report)
	} else {
		fmt.Println(*action + " complete")
	}
}
