package main

import (
	"context"
	"sync"

	"delightd/pkg/furnish"
	"delightd/pkg/kube"
)

// cluster is furnish's handle on the live cluster: converge (apply), remove (delete), and
// read (health) one piece, over the shared pkg/furnish operations. Injectable so the CLI
// verbs are tested against a fake cluster, no client-go client required.
type cluster interface {
	apply(ctx context.Context, pieceDir string) error
	remove(ctx context.Context, pieceDir string) error
	health(ctx context.Context, pieceDir string) ([]furnish.Item, error)
}

// kubeCluster is the production cluster over a client-go client, built lazily on first use
// so the command tree constructs even with no kubeconfig present.
type kubeCluster struct {
	once sync.Once
	c    *kube.Client
	err  error
}

func (k *kubeCluster) client() (*kube.Client, error) {
	k.once.Do(func() { k.c, k.err = kube.FromKubeconfig("") })
	return k.c, k.err
}

func (k *kubeCluster) apply(ctx context.Context, pieceDir string) error {
	c, err := k.client()
	if err != nil {
		return err
	}
	return furnish.Up(ctx, c, pieceDir)
}

func (k *kubeCluster) remove(ctx context.Context, pieceDir string) error {
	c, err := k.client()
	if err != nil {
		return err
	}
	return furnish.Down(ctx, c, pieceDir)
}

func (k *kubeCluster) health(ctx context.Context, pieceDir string) ([]furnish.Item, error) {
	c, err := k.client()
	if err != nil {
		return nil, err
	}
	return furnish.Health(ctx, c, pieceDir)
}
