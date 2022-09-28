package config

import "sigs.k8s.io/controller-runtime/pkg/log"

const controllerName = "config-controller"

var logger = log.Log.WithName(controllerName)
