package policyreport

import (
	"context"
	"crypto/rand"
	"math/big"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/kyverno/kyverno/pkg/client/clientset/versioned"
	"github.com/kyverno/kyverno/pkg/config"
	"github.com/kyverno/kyverno/pkg/toggle"
	"github.com/patrickmn/go-cache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// creator is an interface that buffers report change requests
// merges and creates requests every tickerInterval
type creator interface {
	add(request *unstructured.Unstructured)
	create(request *unstructured.Unstructured) error
	run(stopChan <-chan struct{})
}

type changeRequestCreator struct {
	client versioned.Interface

	// addCache preserves requests that are to be added to report
	rcr_cache *cache.Cache

	crcr_cache *cache.Cache
	// removeCache preserves requests that are to be removed from report
	// removeCache *cache.Cache
	mutex sync.RWMutex
	queue []string

	tickerInterval time.Duration

	log logr.Logger
}

func newChangeRequestCreator(client versioned.Interface, tickerInterval time.Duration, log logr.Logger) creator {
	return &changeRequestCreator{
		client:         client,
		rcr_cache:      cache.New(0, 24*time.Hour),
		crcr_cache:     cache.New(0, 24*time.Hour),
		queue:          []string{},
		tickerInterval: tickerInterval,
		log:            log,
	}
}

func (c *changeRequestCreator) add(request *unstructured.Unstructured) {
	uid, _ := rand.Int(rand.Reader, big.NewInt(100000))
	var err error

	switch request.GetKind() {
	case "ClusterReportChangeRequest":
		err = c.crcr_cache.Add(uid.String(), request, cache.NoExpiration)
		if err != nil {
			c.log.Error(err, "failed to add ClusterReportChangeRequest to cache, replacing", "cache length", c.crcr_cache.ItemCount())
			if err = c.crcr_cache.Replace(uid.String(), request, cache.NoExpiration); err != nil {
				c.log.Error(err, "failed to replace CRCR")
				return
			}
		}
	case "ReportChangeRequest":
		err = c.rcr_cache.Add(uid.String(), request, cache.NoExpiration)
		if err != nil {
			c.log.Error(err, "failed to add ReportChangeRequest to cache, replacing", "cache length", c.rcr_cache.ItemCount())
			if err = c.rcr_cache.Replace(uid.String(), request, cache.NoExpiration); err != nil {
				c.log.Error(err, "failed to replace RCR")
				return
			}
		}
	default:
		return
	}

	c.mutex.Lock()
	c.queue = append(c.queue, uid.String())
	c.mutex.Unlock()
}

func (c *changeRequestCreator) create(request *unstructured.Unstructured) error {
	var ns string
	if request.GetKind() == "ReportChangeRequest" {
		ns = config.KyvernoNamespace()
		rcr, err := convertToRCR(request)
		if err != nil {
			return err
		}

		_, err = c.client.KyvernoV1alpha2().ReportChangeRequests(ns).Create(context.TODO(), rcr, metav1.CreateOptions{})
		return err
	}

	crcr, err := convertToCRCR(request)
	if err != nil {
		return err
	}

	_, err = c.client.KyvernoV1alpha2().ClusterReportChangeRequests().Create(context.TODO(), crcr, metav1.CreateOptions{})
	return err
}

func (c *changeRequestCreator) run(stopChan <-chan struct{}) {
	ticker := time.NewTicker(c.tickerInterval)
	defer ticker.Stop()

	if toggle.SplitPolicyReport.Enabled() {
		err := CleanupPolicyReport(c.client)
		if err != nil {
			c.log.Error(err, "failed to delete old reports")
		}
	}

	for {
		select {
		case <-ticker.C:
			var requests []*unstructured.Unstructured
			var size int
			if toggle.SplitPolicyReport.Enabled() {
				requests, size = c.mergeRequestsPerPolicy()
			} else {
				requests, size = c.mergeRequests()
			}
			for _, request := range requests {
				if err := c.create(request); err != nil {
					c.log.Error(err, "failed to create report change request", "req", request.Object)
				}
			}

			c.cleanupQueue(size)
		case <-stopChan:
			return
		}
	}
}

func (c *changeRequestCreator) cleanupQueue(size int) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	for i := 0; i < size; i++ {
		uid := c.queue[i]
		c.crcr_cache.Delete(uid)
		c.rcr_cache.Delete(uid)
	}

	c.queue = c.queue[size:]
}

// mergeRequests merges all current cached requests
// it blocks writing to the cache
func (c *changeRequestCreator) mergeRequests() (results []*unstructured.Unstructured, size int) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	mergedCRCR := &unstructured.Unstructured{}
	mergedRCR := make(map[string]*unstructured.Unstructured)
	size = len(c.queue)

	for _, uid := range c.queue {
		if unstr, ok := c.crcr_cache.Get(uid); ok {
			if crcr, ok := unstr.(*unstructured.Unstructured); ok {
				if isDeleteRequest(crcr) {
					if !reflect.DeepEqual(mergedCRCR, &unstructured.Unstructured{}) {
						results = append(results, mergedCRCR)
						mergedCRCR = &unstructured.Unstructured{}
					}

					results = append(results, crcr)
				} else {
					if reflect.DeepEqual(mergedCRCR, &unstructured.Unstructured{}) {
						mergedCRCR = crcr
						continue
					}

					if ok := merge(mergedCRCR, crcr); !ok {
						results = append(results, mergedCRCR)
						mergedCRCR = crcr
					}
				}
			}
			continue
		}

		if unstr, ok := c.rcr_cache.Get(uid); ok {
			if rcr, ok := unstr.(*unstructured.Unstructured); ok {
				resourceNS := rcr.GetLabels()[ResourceLabelNamespace]
				mergedNamespacedRCR, ok := mergedRCR[resourceNS]
				if !ok {
					mergedNamespacedRCR = &unstructured.Unstructured{}
				}

				if isDeleteRequest(rcr) {
					if !reflect.DeepEqual(mergedNamespacedRCR, &unstructured.Unstructured{}) {
						results = append(results, mergedNamespacedRCR)
						mergedRCR[resourceNS] = &unstructured.Unstructured{}
					}

					results = append(results, rcr)
				} else {
					if reflect.DeepEqual(mergedNamespacedRCR, &unstructured.Unstructured{}) {
						mergedRCR[resourceNS] = rcr
						continue
					}

					if ok := merge(mergedNamespacedRCR, rcr); !ok {
						results = append(results, mergedNamespacedRCR)
						mergedRCR[resourceNS] = rcr
					} else {
						mergedRCR[resourceNS] = mergedNamespacedRCR
					}
				}
			}
		}
	}

	if !reflect.DeepEqual(mergedCRCR, &unstructured.Unstructured{}) {
		results = append(results, mergedCRCR)
	}

	for _, mergedNamespacedRCR := range mergedRCR {
		if !reflect.DeepEqual(mergedNamespacedRCR, &unstructured.Unstructured{}) {
			results = append(results, mergedNamespacedRCR)
		}
	}
	return
}

// mergeRequests merges all current cached requests per policy
// it blocks writing to the cache
func (c *changeRequestCreator) mergeRequestsPerPolicy() (results []*unstructured.Unstructured, size int) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	mergedCRCR := make(map[string]*unstructured.Unstructured)
	mergedRCR := make(map[string]*unstructured.Unstructured)
	size = len(c.queue)

	for _, uid := range c.queue {
		if unstr, ok := c.crcr_cache.Get(uid); ok {
			if crcr, ok := unstr.(*unstructured.Unstructured); ok {
				policyName := crcr.GetLabels()[policyLabel]
				mergedPolicyCRCR, ok := mergedCRCR[policyName]
				if !ok {
					mergedPolicyCRCR = &unstructured.Unstructured{}
				}

				if isDeleteRequest(crcr) {
					if !reflect.DeepEqual(mergedPolicyCRCR, &unstructured.Unstructured{}) {
						results = append(results, mergedPolicyCRCR)
						mergedCRCR[policyName] = &unstructured.Unstructured{}
					}

					results = append(results, crcr)
				} else {
					if reflect.DeepEqual(mergedPolicyCRCR, &unstructured.Unstructured{}) {
						mergedCRCR[policyName] = crcr
						continue
					}

					if ok := merge(mergedPolicyCRCR, crcr); !ok {
						results = append(results, mergedPolicyCRCR)
						mergedCRCR[policyName] = crcr
					} else {
						mergedCRCR[policyName] = mergedPolicyCRCR
					}
				}
			}
			continue
		}

		if unstr, ok := c.rcr_cache.Get(uid); ok {
			if rcr, ok := unstr.(*unstructured.Unstructured); ok {
				policyName := rcr.GetLabels()[policyLabel]
				resourceNS := rcr.GetLabels()[ResourceLabelNamespace]
				mergedNamespacedRCR, ok := mergedRCR[policyName+resourceNS]
				if !ok {
					mergedNamespacedRCR = &unstructured.Unstructured{}
				}

				if isDeleteRequest(rcr) {
					if !reflect.DeepEqual(mergedNamespacedRCR, &unstructured.Unstructured{}) {
						results = append(results, mergedNamespacedRCR)
						mergedRCR[policyName+resourceNS] = &unstructured.Unstructured{}
					}

					results = append(results, rcr)
				} else {
					if reflect.DeepEqual(mergedNamespacedRCR, &unstructured.Unstructured{}) {
						mergedRCR[policyName+resourceNS] = rcr
						continue
					}

					if ok := merge(mergedNamespacedRCR, rcr); !ok {
						results = append(results, mergedNamespacedRCR)
						mergedRCR[policyName+resourceNS] = rcr
					} else {
						mergedRCR[policyName+resourceNS] = mergedNamespacedRCR
					}
				}
			}
		}
	}

	for _, mergedPolicyCRCR := range mergedCRCR {
		if !reflect.DeepEqual(mergedPolicyCRCR, &unstructured.Unstructured{}) {
			results = append(results, mergedPolicyCRCR)
		}
	}

	for _, mergedNamespacedRCR := range mergedRCR {
		if !reflect.DeepEqual(mergedNamespacedRCR, &unstructured.Unstructured{}) {
			results = append(results, mergedNamespacedRCR)
		}
	}
	return
}

// merge merges elements from a source object into a
// destination object if they share the same namespace label
func merge(dst, src *unstructured.Unstructured) bool {
	dstNS := dst.GetLabels()[ResourceLabelNamespace]
	srcNS := src.GetLabels()[ResourceLabelNamespace]
	if dstNS != srcNS {
		return false
	}

	if dstResults, ok, _ := unstructured.NestedSlice(dst.UnstructuredContent(), "results"); ok {
		if srcResults, ok, _ := unstructured.NestedSlice(src.UnstructuredContent(), "results"); ok {
			dstResults = append(dstResults, srcResults...)

			if err := unstructured.SetNestedSlice(dst.UnstructuredContent(), dstResults, "results"); err == nil {
				err = addSummary(dst, src)
				return err == nil
			}
		}
	}
	return false
}

func addSummary(dst, src *unstructured.Unstructured) error {
	if dstSum, ok, _ := unstructured.NestedMap(dst.UnstructuredContent(), "summary"); ok {
		if srcSum, ok, _ := unstructured.NestedMap(src.UnstructuredContent(), "summary"); ok {
			for key, dstVal := range dstSum {
				if dstValInt, ok := dstVal.(int64); ok {
					if srcVal, ok := srcSum[key].(int64); ok {
						dstSum[key] = dstValInt + srcVal
					}
				}
			}
		}
		return unstructured.SetNestedMap(dst.UnstructuredContent(), dstSum, "summary")
	}
	return nil
}

func isDeleteRequest(request *unstructured.Unstructured) bool {
	deleteLabels := []string{deletedLabelPolicy, deletedLabelRule}
	labels := request.GetLabels()

	for _, l := range deleteLabels {
		if _, ok := labels[l]; ok {
			return true
		}
	}

	deleteAnnotations := []string{deletedAnnotationResourceName, deletedAnnotationResourceKind}
	annotations := request.GetAnnotations()
	for _, ann := range deleteAnnotations {
		if _, ok := annotations[ann]; ok {
			return true
		}
	}

	return false
}
