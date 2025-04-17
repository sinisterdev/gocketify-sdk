package main

import (
	"log"

	rocketify "github.com/sinisterdev/gocketify-sdk"
)

func main() {
	// Create a new SDK instance
	sdk := rocketify.New()

	// Configure the SDK
	opts := rocketify.Options{
		APIKey: "your_api_key",
		Debug:  true,
	}

	// Initialize the SDK
	if _, err := sdk.Init(opts); err != nil {
		log.Fatal(err)
	}

	// Make sure to clean up when done
	defer sdk.Close()

	// Example usage
	sdk.Log("Hello, Rocketify!", rocketify.LogTypeInfo)

	// Send a notification
	sdk.Notify(rocketify.GenericNotification{
		Title:   "Test Notification",
		Message: "This is a test notification.",
	})
}
