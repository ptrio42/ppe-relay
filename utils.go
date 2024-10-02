package main

import (
	"errors"
	"github.com/nbd-wtf/go-nostr"
	"log"
	"os"
)

func GetEnv(key string) string {
	value, exists := os.LookupEnv(key)
	if !exists {
		log.Fatalf("Environment variable %s not set", key)
	}
	return value
}

func ValueFromTag(event *nostr.Event, key string) (*string, error) {
	for _, tag := range event.Tags {
		if tag[0] == key {
			return &tag[1], nil
		}
	}
	return nil, errors.New("tag not found")
}
