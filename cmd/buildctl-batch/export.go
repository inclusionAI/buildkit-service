package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func runExport(opts options) error {
	resetCommandStartTime(time.Now())

	db, err := openLMDBResultDB(opts.fromResultPath)
	if err != nil {
		return fmt.Errorf("open result database: %w", err)
	}
	defer db.Close()

	entries, err := db.All()
	if err != nil {
		return err
	}

	exportableCount := 0
	for _, entry := range entries {
		if !targetMatchesMode(entry.Target, opts.oci) {
			continue
		}
		if !entry.Success && !opts.withFail {
			continue
		}
		exportableCount++
	}
	resetLogProgress(exportableCount)
	defer clearLogProgress()

	file, err := os.Create(opts.resultPath)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	var exported int
	for _, entry := range entries {
		if !targetMatchesMode(entry.Target, opts.oci) {
			continue
		}
		if !entry.Success && !opts.withFail {
			continue
		}
		payload, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		writer.Write(payload)
		writer.WriteByte('\n')
		exported++
		advanceLogProgress()
	}

	if err := writer.Flush(); err != nil {
		return err
	}

	logInfo("Exported %d entries to %s", exported, opts.resultPath)
	return nil
}
