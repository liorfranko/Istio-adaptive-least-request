package controller

import "sigs.k8s.io/controller-runtime/pkg/client"

func isObjectMarkedForDeletion(obj client.Object) bool {
	return !obj.GetDeletionTimestamp().IsZero()
}
