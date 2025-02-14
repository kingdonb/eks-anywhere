package validations

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aws/eks-anywhere/pkg/api/v1alpha1"
)

func ValidateClusterNameArg(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("please specify a cluster name")
	}
	err := v1alpha1.ValidateClusterName(args[0])
	if err != nil {
		return args[0], err
	}
	err = v1alpha1.ValidateClusterNameLength(args[0])
	if err != nil {
		return args[0], err
	}
	return args[0], nil
}

func FileExists(filename string) bool {
	_, err := os.Stat(filename)
	return !os.IsNotExist(err)
}

func KubeConfigExists(dir, clusterName string, kubeConfigFileOverride string, kubeconfigPattern string) bool {
	kubeConfigFile := kubeConfigFileOverride
	if kubeConfigFile == "" {
		kubeConfigFile = filepath.Join(dir, fmt.Sprintf(kubeconfigPattern, clusterName))
	}

	if info, err := os.Stat(kubeConfigFile); err == nil && info.Size() > 0 {
		return true
	}
	return false
}
