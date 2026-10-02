package main

import (
	"context"

	"Coves/internal/atproto/identity"
	"Coves/internal/config"
	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"

	imageproxyhandlers "Coves/internal/api/handlers/imageproxy"
)

// imageProxyDependencies are the seams the image proxy service and handler
// are built from.
type imageProxyDependencies struct {
	cache     imageproxy.Cache
	processor imageproxy.Processor
	fetcher   imageproxy.Fetcher
	blocks    imageproxy.BlockChecker
	lister    imageproxy.BlockedBlobLister
	resolver  identity.Resolver
}

// buildImageProxyService installs the recorder before the startup sweep runs.
func buildImageProxyService(cfg imageproxy.Config, purge config.CDNPurgeConfig, queue *moderation.CDNPurgeQueue, dependencies imageProxyDependencies) (*imageproxy.ImageProxyService, *imageproxyhandlers.Handler, context.CancelFunc, error) {
	var serviceOptions []imageproxy.ServiceOption
	var handlerOptions []imageproxyhandlers.HandlerOption
	if queue != nil {
		serviceOptions = append(serviceOptions, imageproxy.WithCDNPurgeRecorder(queue))
		handlerOptions = append(handlerOptions, imageproxyhandlers.WithCDNPurgeBaseURLs(purge.BaseURLs))
	}
	service, err := imageproxy.NewService(dependencies.cache, dependencies.processor, dependencies.fetcher, dependencies.blocks, cfg, serviceOptions...)
	if err != nil {
		return nil, nil, nil, err
	}
	stopPurge := service.StartActiveBlockPurgeJob(dependencies.lister, cfg.CleanupInterval)
	handler := imageproxyhandlers.NewHandler(service, dependencies.resolver, handlerOptions...)
	return service, handler, stopPurge, nil
}
