package common

import (
	"context"
	"fmt"

	"github.com/werf/kubedog/pkg/kube"
)

var ondemandKubeInitializer *OndemandKubeInitializer

type OndemandKubeInitializer struct {
	KubeContext      string
	KubeConfig       string
	KubeConfigBase64 string

	initialized bool
}

func SetupOndemandKubeInitializer(kubeContext, kubeConfig, kubeConfigBase64 string) {
	ondemandKubeInitializer = &OndemandKubeInitializer{
		KubeContext:      kubeContext,
		KubeConfig:       kubeConfig,
		KubeConfigBase64: kubeConfigBase64,
	}
}

func GetOndemandKubeInitializer() *OndemandKubeInitializer {
	return ondemandKubeInitializer
}

func (initializer *OndemandKubeInitializer) Init(ctx context.Context) error {
	if initializer.initialized {
		return nil
	}

	if err := kube.Init(kube.InitOptions{KubeConfigOptions: kube.KubeConfigOptions{
		Context:          initializer.KubeContext,
		ConfigPath:       initializer.KubeConfig,
		ConfigDataBase64: initializer.KubeConfigBase64,
	}}); err != nil {
		return fmt.Errorf("cannot initialize kube: %s", err)
	}

	if err := InitKubedog(ctx); err != nil {
		return fmt.Errorf("cannot init kubedog: %s", err)
	}

	initializer.initialized = true

	return nil
}
