package wellknown

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
)

// HandleAppleAppSiteAssociation serves the iOS Universal Links configuration
// GET /.well-known/apple-app-site-association
//
// Universal Links provide cryptographic binding between the app and domain:
// - Requires apple-app-site-association file served over HTTPS
// - App must have Associated Domains capability configured
// - System verifies domain ownership before routing deep links
// - Prevents malicious apps from intercepting deep links
//
// Spec: https://developer.apple.com/documentation/xcode/supporting-universal-links-in-your-app
func HandleAppleAppSiteAssociation(w http.ResponseWriter, r *http.Request) {
	// Get Apple App ID from environment (format: <Team ID>.<Bundle ID>)
	// Example: "ABCD1234.social.coves.app"
	// Find Team ID in Apple Developer Portal -> Membership
	// Bundle ID is configured in Xcode project
	appleAppID := os.Getenv("APPLE_APP_ID")
	if appleAppID == "" {
		// Development fallback - allows testing without real Team ID
		// IMPORTANT: This MUST be set in production for Universal Links to work
		appleAppID = "DEVELOPMENT.social.coves.app"
		slog.Warn("APPLE_APP_ID not set, using development placeholder",
			"app_id", appleAppID,
			"note", "Set APPLE_APP_ID env var for production Universal Links")
	}

	// Apple requires application/json content type (no charset)
	w.Header().Set("Content-Type", "application/json")

	// Construct the response per Apple's spec
	// See: https://developer.apple.com/documentation/bundleresources/applinks
	response := map[string]interface{}{
		"applinks": map[string]interface{}{
			"apps": []string{}, // Must be empty array per Apple spec
			"details": []map[string]interface{}{
				{
					"appID": appleAppID,
					// Paths that trigger Universal Links when opened in Safari/other apps
					// These URLs will open the app instead of the browser
					"paths": []string{
						"/app/oauth/callback",   // Primary Universal Link OAuth callback
						"/app/oauth/callback/*", // Catch-all for query params
					},
				},
			},
		},
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		slog.Error("failed to encode apple-app-site-association", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Debug("served apple-app-site-association", "app_id", appleAppID)
}

// HandleAssetLinks serves the Android App Links configuration
// GET /.well-known/assetlinks.json
//
// App Links provide cryptographic binding between the app and domain:
// - Requires assetlinks.json file served over HTTPS
// - App must have intent-filter with android:autoVerify="true"
// - System verifies domain ownership via SHA-256 certificate fingerprint
// - Prevents malicious apps from intercepting deep links
//
// Spec: https://developer.android.com/training/app-links/verify-android-applinks
func HandleAssetLinks(w http.ResponseWriter, r *http.Request) {
	// Get Android package name from environment
	// Example: "social.coves.app"
	androidPackage := os.Getenv("ANDROID_PACKAGE_NAME")
	if androidPackage == "" {
		androidPackage = "social.coves.app" // Default for development
		slog.Warn("ANDROID_PACKAGE_NAME not set, using default",
			"package", androidPackage,
			"note", "Set ANDROID_PACKAGE_NAME env var for production App Links")
	}

	// Get SHA-256 fingerprint from environment
	// This is the SHA-256 fingerprint of the app's signing certificate
	//
	// To get the fingerprint:
	// Production: keytool -list -v -keystore release.jks -alias release
	// Debug: keytool -list -v -keystore ~/.android/debug.keystore -alias androiddebugkey -storepass android -keypass android
	//
	// Look for "SHA256:" in the output
	// Format: AA:BB:CC:DD:...:FF (64 hex characters separated by colons)
	androidFingerprint := os.Getenv("ANDROID_SHA256_FINGERPRINT")
	if androidFingerprint == "" {
		// Development fallback - this won't work for real App Links verification
		// IMPORTANT: This MUST be set in production for App Links to work
		androidFingerprint = "00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00"
		slog.Warn("ANDROID_SHA256_FINGERPRINT not set, using development placeholder",
			"fingerprint", androidFingerprint,
			"note", "Set ANDROID_SHA256_FINGERPRINT env var for production App Links")
	}

	w.Header().Set("Content-Type", "application/json")

	// Construct the response per Google's Digital Asset Links spec
	// See: https://developers.google.com/digital-asset-links/v1/getting-started
	response := []map[string]interface{}{
		{
			// delegate_permission/common.handle_all_urls grants the app permission
			// to handle URLs for this domain
			"relation": []string{"delegate_permission/common.handle_all_urls"},
			"target": map[string]interface{}{
				"namespace":    "android_app",
				"package_name": androidPackage,
				// List of certificate fingerprints that can sign the app
				// Multiple fingerprints can be provided for different signing keys
				// (e.g., debug + release)
				"sha256_cert_fingerprints": []string{
					androidFingerprint,
				},
			},
		},
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		slog.Error("failed to encode assetlinks.json", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Debug("served assetlinks.json",
		"package", androidPackage,
		"fingerprint", androidFingerprint)
}
