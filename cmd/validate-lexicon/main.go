package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
)

func main() {
	var (
		schemaPath   = flag.String("path", "internal/atproto/lexicon", "Path to lexicon schemas directory")
		testDataPath = flag.String("test-data", "tests/lexicon-test-data", "Path to test data directory for ValidateRecord testing")
		verbose      = flag.Bool("v", false, "Verbose output")
		strict       = flag.Bool("strict", false, "Use strict validation mode")
		schemasOnly  = flag.Bool("schemas-only", false, "Only validate schemas, skip test data validation")
	)
	flag.Parse()

	if *verbose {
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}

	// Check if path exists
	if _, err := os.Stat(*schemaPath); os.IsNotExist(err) {
		log.Fatalf("Schema path does not exist: %s", *schemaPath)
	}

	// Create a new catalog
	catalog := lexicon.NewBaseCatalog()

	// Load all schemas from the directory
	fmt.Printf("Loading schemas from: %s\n", *schemaPath)
	if err := loadSchemasWithDebug(&catalog, *schemaPath, *verbose); err != nil {
		log.Fatalf("Failed to load schemas: %v", err)
	}

	fmt.Printf("✅ Successfully loaded schemas from %s\n", *schemaPath)

	// Validate schema structure by trying to resolve some known schemas
	if err := validateSchemaStructure(&catalog, *schemaPath, *verbose); err != nil {
		log.Fatalf("Schema validation failed: %v", err)
	}

	// Validate cross-references between schemas
	if err := validateCrossReferences(&catalog, *verbose); err != nil {
		log.Fatalf("Cross-reference validation failed: %v", err)
	}

	// Validate test data unless schemas-only flag is set
	if !*schemasOnly {
		fmt.Printf("\n📋 Validating test data from: %s\n", *testDataPath)
		allSchemas := extractAllSchemaIDs(*schemaPath)
		if err := validateTestData(&catalog, *testDataPath, *verbose, *strict, allSchemas); err != nil {
			log.Fatalf("Test data validation failed: %v", err)
		}
	} else {
		fmt.Println("\n⏩ Skipping test data validation (--schemas-only flag set)")
	}

	fmt.Println("\n✅ All validations passed successfully!")
}

// validateSchemaStructure performs additional validation checks
func validateSchemaStructure(catalog *lexicon.BaseCatalog, schemaPath string, verbose bool) error {
	var validationErrors []string
	var schemaFiles []string
	var schemaIDs []string

	// Collect all JSON schema files and derive their IDs
	err := filepath.Walk(schemaPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip test-data directory
		if info.IsDir() && info.Name() == "test-data" {
			return filepath.SkipDir
		}

		// Only process .json files
		if !info.IsDir() && filepath.Ext(path) == ".json" {
			schemaFiles = append(schemaFiles, path)

			// Convert file path to schema ID
			// e.g., internal/atproto/lexicon/social/coves/actor/profile.json -> social.coves.actor.profile
			relPath, err := filepath.Rel(schemaPath, path)
			if err != nil {
				return fmt.Errorf("failed to compute relative path: %w", err)
			}
			schemaID := filepath.ToSlash(relPath)
			schemaID = schemaID[:len(schemaID)-5] // Remove .json extension
			schemaID = strings.ReplaceAll(schemaID, "/", ".")
			schemaIDs = append(schemaIDs, schemaID)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("error walking schema directory: %w", err)
	}

	if verbose {
		fmt.Printf("\nFound %d schema files to validate:\n", len(schemaFiles))
		for _, file := range schemaFiles {
			fmt.Printf("  - %s\n", file)
		}
	}

	// Validate all discovered schemas
	if verbose {
		fmt.Println("\nValidating all schemas:")
	}

	for i, schemaID := range schemaIDs {
		if _, err := catalog.Resolve(schemaID); err != nil {
			validationErrors = append(validationErrors, fmt.Sprintf("Failed to resolve schema %s (from %s): %v", schemaID, schemaFiles[i], err))
		} else if verbose {
			fmt.Printf("  ✅ %s\n", schemaID)
		}
	}

	if len(validationErrors) > 0 {
		fmt.Println("❌ Schema validation errors found:")
		for _, errMsg := range validationErrors {
			fmt.Printf("  %s\n", errMsg)
		}
		return fmt.Errorf("found %d validation errors", len(validationErrors))
	}

	fmt.Printf("\n✅ Successfully validated all %d schemas\n", len(schemaIDs))
	return nil
}

// loadSchemasWithDebug loads schemas one by one to identify problematic files
func loadSchemasWithDebug(catalog *lexicon.BaseCatalog, schemaPath string, verbose bool) error {
	var schemaFiles []string

	// Collect all JSON schema files
	err := filepath.Walk(schemaPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip test-data directory
		if info.IsDir() && info.Name() == "test-data" {
			return filepath.SkipDir
		}

		// Only process .json files
		if !info.IsDir() && filepath.Ext(path) == ".json" {
			schemaFiles = append(schemaFiles, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("error walking schema directory: %w", err)
	}

	// Try to load schemas one by one
	for _, schemaFile := range schemaFiles {
		if verbose {
			fmt.Printf("  Loading: %s\n", schemaFile)
		}

		// Create a temporary catalog for this file
		tempCatalog := lexicon.NewBaseCatalog()
		if err := tempCatalog.LoadDirectory(filepath.Dir(schemaFile)); err != nil {
			return fmt.Errorf("failed to load schema file %s: %w", schemaFile, err)
		}
	}

	// If all individual files loaded OK, try loading the whole directory
	return catalog.LoadDirectory(schemaPath)
}

// extractAllSchemaIDs walks the schema directory and returns all schema IDs
func extractAllSchemaIDs(schemaPath string) []string {
	var schemaIDs []string

	if err := filepath.Walk(schemaPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip test-data directory
		if info.IsDir() && info.Name() == "test-data" {
			return filepath.SkipDir
		}

		// Only process .json files
		if !info.IsDir() && filepath.Ext(path) == ".json" {
			// Convert file path to schema ID
			relPath, err := filepath.Rel(schemaPath, path)
			if err != nil {
				return err
			}
			schemaID := filepath.ToSlash(relPath)
			schemaID = schemaID[:len(schemaID)-5] // Remove .json extension
			schemaID = strings.ReplaceAll(schemaID, "/", ".")

			// Only include record schemas (not procedures)
			if strings.Contains(schemaID, ".record") ||
				strings.Contains(schemaID, ".profile") ||
				strings.Contains(schemaID, ".rules") ||
				strings.Contains(schemaID, ".wiki") ||
				strings.Contains(schemaID, ".subscription") ||
				strings.Contains(schemaID, ".membership") ||
				strings.Contains(schemaID, ".vote") ||
				strings.Contains(schemaID, ".tag") ||
				strings.Contains(schemaID, ".comment") ||
				strings.Contains(schemaID, ".share") ||
				strings.Contains(schemaID, ".tribunalVote") ||
				strings.Contains(schemaID, ".ruleProposal") ||
				strings.Contains(schemaID, ".ban") {
				schemaIDs = append(schemaIDs, schemaID)
			}
		}
		return nil
	}); err != nil {
		log.Printf("Warning: failed to walk schema directory: %v", err)
	}

	return schemaIDs
}

// validateTestData validates test JSON data files against their corresponding schemas
func validateTestData(catalog *lexicon.BaseCatalog, testDataPath string, verbose, strict bool, allSchemas []string) error {
	// Check if test data directory exists
	if _, err := os.Stat(testDataPath); os.IsNotExist(err) {
		return fmt.Errorf("test data path does not exist: %s", testDataPath)
	}

	var validationErrors []string
	validFiles := 0
	invalidFiles := 0
	validSuccessCount := 0
	invalidFailCount := 0
	testedTypes := make(map[string]bool)

	// Walk through test data directory
	err := filepath.Walk(testDataPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Only process .json files
		if !info.IsDir() && filepath.Ext(path) == ".json" {
			filename := filepath.Base(path)
			isInvalidTest := strings.Contains(filename, "-invalid-")

			if verbose {
				if isInvalidTest {
					fmt.Printf("\n  Testing (expect failure): %s\n", filename)
				} else {
					fmt.Printf("\n  Testing: %s\n", filename)
				}
			}

			// Read the test file
			file, err := os.Open(path)
			if err != nil {
				validationErrors = append(validationErrors, fmt.Sprintf("Failed to open %s: %v", path, err))
				return nil
			}
			defer func() {
				if closeErr := file.Close(); closeErr != nil {
					validationErrors = append(validationErrors, fmt.Sprintf("Failed to close %s: %v", path, closeErr))
				}
			}()

			data, readErr := io.ReadAll(file)
			if readErr != nil {
				validationErrors = append(validationErrors, fmt.Sprintf("Failed to read %s: %v", path, readErr))
				return nil
			}

			// Parse JSON data using Decoder to handle numbers properly
			var recordData map[string]interface{}
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber() // This preserves numbers as json.Number instead of float64
			if decodeErr := decoder.Decode(&recordData); decodeErr != nil {
				validationErrors = append(validationErrors, fmt.Sprintf("Failed to parse JSON in %s: %v", path, decodeErr))
				return nil
			}

			// Convert json.Number values to appropriate types
			recordData = convertNumbers(recordData).(map[string]interface{})

			// Extract $type field
			recordType, ok := recordData["$type"].(string)
			if !ok {
				validationErrors = append(validationErrors, fmt.Sprintf("Missing or invalid $type field in %s", path))
				return nil
			}

			// Set validation flags
			flags := lexicon.ValidateFlags(0)
			if strict {
				flags |= lexicon.StrictRecursiveValidation
			} else {
				flags |= lexicon.AllowLenientDatetime
			}

			// Validate the record
			validateErr := lexicon.ValidateRecord(catalog, recordData, recordType, flags)

			if isInvalidTest {
				// This file should fail validation
				invalidFiles++
				if validateErr != nil {
					invalidFailCount++
					if verbose {
						fmt.Printf("    ✅ Correctly rejected invalid %s record: %v\n", recordType, validateErr)
					}
				} else {
					validationErrors = append(validationErrors, fmt.Sprintf("Invalid test file %s passed validation when it should have failed", path))
					if verbose {
						fmt.Printf("    ❌ ERROR: Invalid record passed validation!\n")
					}
				}
			} else {
				// This file should pass validation
				validFiles++
				if validateErr != nil {
					validationErrors = append(validationErrors, fmt.Sprintf("Validation failed for %s (type: %s): %v", path, recordType, validateErr))
					if verbose {
						fmt.Printf("    ❌ Failed: %v\n", validateErr)
					}
				} else {
					validSuccessCount++
					testedTypes[recordType] = true
					if verbose {
						fmt.Printf("    ✅ Valid %s record\n", recordType)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("error walking test data directory: %w", err)
	}

	if len(validationErrors) > 0 {
		fmt.Println("\n❌ Test data validation errors found:")
		for _, errMsg := range validationErrors {
			fmt.Printf("  %s\n", errMsg)
		}
		return fmt.Errorf("found %d validation errors", len(validationErrors))
	}

	totalFiles := validFiles + invalidFiles
	if totalFiles == 0 {
		fmt.Println("  ⚠️  No test data files found")
	} else {
		// Show validation summary
		fmt.Printf("\n📋 Validation Summary:\n")
		fmt.Printf("  Valid test files:   %d/%d passed\n", validSuccessCount, validFiles)
		fmt.Printf("  Invalid test files: %d/%d correctly rejected\n", invalidFailCount, invalidFiles)

		if validSuccessCount == validFiles && invalidFailCount == invalidFiles {
			fmt.Printf("\n  ✅ All test files behaved as expected!\n")
		}

		// Show test coverage summary (only for valid files)
		fmt.Printf("\n📊 Test Data Coverage Summary:\n")
		fmt.Printf("  - Records with test data: %d types\n", len(testedTypes))
		fmt.Printf("  - Valid test files: %d\n", validFiles)
		fmt.Printf("  - Invalid test files: %d (for error validation)\n", invalidFiles)

		fmt.Printf("\n  Tested record types:\n")
		for recordType := range testedTypes {
			fmt.Printf("    ✓ %s\n", recordType)
		}

		// Show untested schemas
		untestedCount := 0
		fmt.Printf("\n  ⚠️  Record types without test data:\n")
		for _, schema := range allSchemas {
			if !testedTypes[schema] {
				fmt.Printf("    - %s\n", schema)
				untestedCount++
			}
		}

		if untestedCount == 0 {
			fmt.Println("    (None - full test coverage!)")
		} else {
			fmt.Printf("\n  Coverage: %d/%d record types have test data (%.1f%%)\n",
				len(testedTypes), len(allSchemas),
				float64(len(testedTypes))/float64(len(allSchemas))*100)
		}
	}
	return nil
}

// validateCrossReferences validates that all schema references resolve correctly
func validateCrossReferences(catalog *lexicon.BaseCatalog, verbose bool) error {
	knownRefs := []string{
		// Rich text facets
		"social.coves.richtext.facet",
		"social.coves.richtext.facet#byteSlice",
		"social.coves.richtext.facet#mention",
		"social.coves.richtext.facet#link",
		"social.coves.richtext.facet#bold",
		"social.coves.richtext.facet#italic",
		"social.coves.richtext.facet#strikethrough",
		"social.coves.richtext.facet#spoiler",

		// Post types and views
		"social.coves.post.get#postView",
		"social.coves.post.get#authorView",
		"social.coves.post.get#communityRef",
		"social.coves.post.get#imageView",
		"social.coves.post.get#videoView",
		"social.coves.post.get#externalView",
		"social.coves.post.get#postStats",
		"social.coves.post.get#viewerState",

		// Post record types
		"social.coves.post.record#originalAuthor",

		// Actor definitions
		"social.coves.actor.profile#geoLocation",

		// Community definitions
		"social.coves.community.rules#rule",
	}

	var errors []string
	if verbose {
		fmt.Println("\n🔍 Validating cross-references between schemas:")
	}

	for _, ref := range knownRefs {
		if _, err := catalog.Resolve(ref); err != nil {
			errors = append(errors, fmt.Sprintf("Failed to resolve reference %s: %v", ref, err))
		} else if verbose {
			fmt.Printf("  ✅ %s\n", ref)
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("cross-reference validation failed:\n%s", strings.Join(errors, "\n"))
	}

	return nil
}

// convertNumbers recursively converts json.Number values to int64 or float64
func convertNumbers(v interface{}) interface{} {
	switch vv := v.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{})
		for k, val := range vv {
			result[k] = convertNumbers(val)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(vv))
		for i, val := range vv {
			result[i] = convertNumbers(val)
		}
		return result
	case json.Number:
		// Try to convert to int64 first
		if i, err := vv.Int64(); err == nil {
			return i
		}
		// If that fails, convert to float64
		if f, err := vv.Float64(); err == nil {
			return f
		}
		// If both fail, return as string
		return vv.String()
	default:
		return v
	}
}
