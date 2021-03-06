package ovn

import (
	"fmt"
	"net"
	"time"

	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
)

const (
	// Annotation used to enable/disable multicast in the namespace
	nsMulticastAnnotation = "k8s.ovn.org/multicast-enabled"
)

func (oc *Controller) syncNamespaces(namespaces []interface{}) {
	expectedNs := make(map[string]bool)
	for _, nsInterface := range namespaces {
		ns, ok := nsInterface.(*kapi.Namespace)
		if !ok {
			klog.Errorf("Spurious object in syncNamespaces: %v", nsInterface)
			continue
		}
		expectedNs[ns.Name] = true
	}

	err := oc.addressSetFactory.ForEachAddressSet(func(addrSetName, namespaceName, nameSuffix string) {
		if nameSuffix == "" && !expectedNs[namespaceName] {
			if err := oc.addressSetFactory.DestroyAddressSetInBackingStore(addrSetName); err != nil {
				klog.Errorf(err.Error())
			}
		}
	})
	if err != nil {
		klog.Errorf("Error in syncing namespaces: %v", err)
	}
}

func (oc *Controller) addPodToNamespace(ns string, portInfo *lpInfo) error {
	nsInfo := oc.getNamespaceLocked(ns)
	if nsInfo == nil {
		return nil
	}
	defer nsInfo.Unlock()

	if err := nsInfo.addressSet.AddIPs(createIPAddressSlice(portInfo.ips)); err != nil {
		return err
	}

	// If multicast is allowed and enabled for the namespace, add the port
	// to the allow policy.
	if oc.multicastSupport && nsInfo.multicastEnabled {
		if err := podAddAllowMulticastPolicy(ns, portInfo); err != nil {
			return err
		}
	}

	return nil
}

func (oc *Controller) deletePodFromNamespace(ns string, portInfo *lpInfo) error {
	nsInfo := oc.getNamespaceLocked(ns)
	if nsInfo == nil {
		return nil
	}
	defer nsInfo.Unlock()

	if err := nsInfo.addressSet.DeleteIPs(createIPAddressSlice(portInfo.ips)); err != nil {
		return err
	}

	// Remove the port from the multicast allow policy.
	if oc.multicastSupport && nsInfo.multicastEnabled {
		if err := podDeleteAllowMulticastPolicy(ns, portInfo); err != nil {
			return err
		}
	}

	return nil
}

func createIPAddressSlice(ips []*net.IPNet) []net.IP {
	ipAddrs := make([]net.IP, 0)
	for _, ip := range ips {
		ipAddrs = append(ipAddrs, ip.IP)
	}
	return ipAddrs
}

// Creates an explicit "allow" policy for multicast traffic within the
// namespace if multicast is enabled. Otherwise, removes the "allow" policy.
// Traffic will be dropped by the default multicast deny ACL.
func (oc *Controller) multicastUpdateNamespace(ns *kapi.Namespace, nsInfo *namespaceInfo) {
	if !oc.multicastSupport {
		return
	}

	enabled := (ns.Annotations[nsMulticastAnnotation] == "true")
	enabledOld := nsInfo.multicastEnabled

	if enabledOld == enabled {
		return
	}

	var err error
	nsInfo.multicastEnabled = enabled
	if enabled {
		err = oc.createMulticastAllowPolicy(ns.Name, nsInfo)
	} else {
		err = deleteMulticastAllowPolicy(ns.Name, nsInfo)
	}
	if err != nil {
		klog.Errorf(err.Error())
		return
	}
}

// Cleans up the multicast policy for this namespace if multicast was
// previously allowed.
func (oc *Controller) multicastDeleteNamespace(ns *kapi.Namespace, nsInfo *namespaceInfo) {
	if nsInfo.multicastEnabled {
		nsInfo.multicastEnabled = false
		if err := deleteMulticastAllowPolicy(ns.Name, nsInfo); err != nil {
			klog.Errorf(err.Error())
		}
	}
}

// updateNamepacePortGroup updates the port_group applied to the namespace. Multiple objects
// that apply network configuration to all pods in a namespace will use the same port group.
// This function ensures that the namespace wide port group will only be created once and
// cleaned up when no object that relies on it exists.
func (nsInfo *namespaceInfo) updateNamespacePortGroup(ns string) error {
	if nsInfo.multicastEnabled {
		if nsInfo.portGroupUUID != "" {
			// Multicast is enabled and the port group exists so there is nothing to do.
			return nil
		}

		// The port group should exist but doesn't so create it
		portGroupUUID, err := createPortGroup(ns, hashedPortGroup(ns))
		if err != nil {
			return fmt.Errorf("failed to create port_group for %s (%v)", ns, err)
		}
		nsInfo.portGroupUUID = portGroupUUID
	} else {
		deletePortGroup(hashedPortGroup(ns))
		nsInfo.portGroupUUID = ""
	}
	return nil
}

// AddNamespace creates corresponding addressset in ovn db
func (oc *Controller) AddNamespace(ns *kapi.Namespace) {
	klog.V(5).Infof("Adding namespace: %s", ns.Name)
	nsInfo := oc.createNamespaceLocked(ns.Name)
	defer nsInfo.Unlock()

	// Get all the pods in the namespace and append their IP to the
	// address_set
	var ips []net.IP
	existingPods, err := oc.watchFactory.GetPods(ns.Name)
	if err != nil {
		klog.Errorf("Failed to get all the pods (%v)", err)
	} else {
		ips = make([]net.IP, 0, len(existingPods))
		for _, pod := range existingPods {
			if pod.Status.PodIP != "" && !pod.Spec.HostNetwork {
				podIPs, err := util.GetAllPodIPs(pod)
				if err != nil {
					klog.Warningf(err.Error())
					continue
				}
				ips = append(ips, podIPs...)
			}
		}
	}

	annotation := ns.Annotations[hotypes.HybridOverlayExternalGw]
	if annotation != "" {
		parsedAnnotation := net.ParseIP(annotation)
		if parsedAnnotation == nil {
			klog.Errorf("Could not parse hybrid overlay external gw annotation")
		} else {
			nsInfo.hybridOverlayExternalGW = parsedAnnotation
		}
	}
	annotation = ns.Annotations[hotypes.HybridOverlayVTEP]
	if annotation != "" {
		parsedAnnotation := net.ParseIP(annotation)
		if parsedAnnotation == nil {
			klog.Errorf("Could not parse hybrid overlay VTEP annotation")
		} else {
			nsInfo.hybridOverlayVTEP = parsedAnnotation
		}
	}

	nsInfo.addressSet, err = oc.addressSetFactory.NewAddressSet(ns.Name, ips)
	if err != nil {
		klog.Errorf(err.Error())
	}

	oc.multicastUpdateNamespace(ns, nsInfo)
}

func (oc *Controller) updateNamespace(old, newer *kapi.Namespace) {
	klog.V(5).Infof("Updating namespace: %s", old.Name)

	nsInfo := oc.getNamespaceLocked(old.Name)
	if nsInfo == nil {
		klog.Warningf("Update event for unknown namespace %q", old.Name)
		return
	}
	defer nsInfo.Unlock()

	annotation := newer.Annotations[hotypes.HybridOverlayExternalGw]
	if annotation != "" {
		parsedAnnotation := net.ParseIP(annotation)
		if parsedAnnotation == nil {
			klog.Errorf("Could not parse hybrid overlay external gw annotation")
		} else {
			nsInfo.hybridOverlayExternalGW = parsedAnnotation
		}
	} else {
		nsInfo.hybridOverlayExternalGW = nil
	}
	annotation = newer.Annotations[hotypes.HybridOverlayVTEP]
	if annotation != "" {
		parsedAnnotation := net.ParseIP(annotation)
		if parsedAnnotation == nil {
			klog.Errorf("Could not parse hybrid overlay VTEP annotation")
		} else {
			nsInfo.hybridOverlayVTEP = parsedAnnotation
		}
	} else {
		nsInfo.hybridOverlayVTEP = nil
	}
	oc.multicastUpdateNamespace(newer, nsInfo)
}

func (oc *Controller) deleteNamespace(ns *kapi.Namespace) {
	klog.V(5).Infof("Deleting namespace: %s", ns.Name)

	nsInfo := oc.deleteNamespaceLocked(ns.Name)
	if nsInfo == nil {
		return
	}
	defer nsInfo.Unlock()

	oc.multicastDeleteNamespace(ns, nsInfo)
}

// waitForNamespaceLocked waits up to 10 seconds for a Namespace to be known; use this
// rather than getNamespaceLocked when calling from a thread where you might be processing
// an event in a namespace before the Namespace factory thread has processed the Namespace
// addition.
func (oc *Controller) waitForNamespaceLocked(namespace string) (*namespaceInfo, error) {
	var nsInfo *namespaceInfo

	err := utilwait.PollImmediate(100*time.Millisecond, 10*time.Second,
		func() (bool, error) {
			nsInfo = oc.getNamespaceLocked(namespace)
			return nsInfo != nil, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("timeout waiting for namespace event")
	}
	return nsInfo, nil
}

// getNamespaceLocked locks namespacesMutex, looks up ns, and (if found), returns it with
// its mutex locked. If ns is not known, nil will be returned
func (oc *Controller) getNamespaceLocked(ns string) *namespaceInfo {
	// Only hold namespacesMutex while reading/modifying oc.namespaces. In particular,
	// we drop namespacesMutex while trying to claim nsInfo.Mutex, because something
	// else might have locked the nsInfo and be doing something slow with it, and we
	// don't want to block all access to oc.namespaces while that's happening.
	oc.namespacesMutex.Lock()
	nsInfo := oc.namespaces[ns]
	oc.namespacesMutex.Unlock()

	if nsInfo == nil {
		return nil
	}
	nsInfo.Lock()

	// Check that the namespace wasn't deleted while we were waiting for the lock
	oc.namespacesMutex.Lock()
	defer oc.namespacesMutex.Unlock()
	if nsInfo != oc.namespaces[ns] {
		nsInfo.Unlock()
		return nil
	}
	return nsInfo
}

// createNamespaceLocked locks namespacesMutex, creates an entry for ns, and returns it
// with its mutex locked.
func (oc *Controller) createNamespaceLocked(ns string) *namespaceInfo {
	oc.namespacesMutex.Lock()
	defer oc.namespacesMutex.Unlock()

	nsInfo := &namespaceInfo{
		networkPolicies:  make(map[string]*namespacePolicy),
		multicastEnabled: false,
	}
	nsInfo.Lock()
	oc.namespaces[ns] = nsInfo

	return nsInfo
}

// deleteNamespaceLocked locks namespacesMutex, finds and deletes ns, and returns the
// namespace, locked.
func (oc *Controller) deleteNamespaceLocked(ns string) *namespaceInfo {
	// The locking here is the same as in getNamespaceLocked

	oc.namespacesMutex.Lock()
	nsInfo := oc.namespaces[ns]
	oc.namespacesMutex.Unlock()

	if nsInfo == nil {
		return nil
	}
	nsInfo.Lock()

	oc.namespacesMutex.Lock()
	defer oc.namespacesMutex.Unlock()
	if nsInfo != oc.namespaces[ns] {
		nsInfo.Unlock()
		return nil
	}
	if err := nsInfo.addressSet.Destroy(); err != nil {
		klog.Errorf(err.Error())
	}
	delete(oc.namespaces, ns)

	return nsInfo
}
