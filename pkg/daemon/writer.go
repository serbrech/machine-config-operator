package daemon

import (
	"encoding/json"
	"fmt"

	"github.com/golang/glog"
	"github.com/openshift/machine-config-operator/pkg/daemon/constants"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
)

const (
	// defaultWriterQueue the number of pending writes to queue
	defaultWriterQueue = 25

	// machineConfigDaemonSSHAccessAnnotationKey is used to mark a node after it has been accessed via SSH
	machineConfigDaemonSSHAccessAnnotationKey = "machineconfiguration.openshift.io/ssh"
	// MachineConfigDaemonSSHAccessValue is the annotation value applied when ssh access is detected
	machineConfigDaemonSSHAccessValue = "accessed"
)

// message wraps a client and responseChannel
type message struct {
	client          corev1.NodeInterface
	lister          corelisterv1.NodeLister
	node            string
	annos           map[string]string
	responseChannel chan error
}

// NodeWriter A single writer to Kubernetes to prevent race conditions
type NodeWriter struct {
	writer chan message
}

// NewNodeWriter Create a new NodeWriter
func NewNodeWriter() *NodeWriter {
	return &NodeWriter{
		writer: make(chan message, defaultWriterQueue),
	}
}

// Run reads from the writer channel and sets the node annotation. It will
// return if the stop channel is closed. Intended to be run via a goroutine.
func (nw *NodeWriter) Run(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case msg := <-nw.writer:
			_, err := setNodeAnnotations(msg.client, msg.lister, msg.node, msg.annos)
			msg.responseChannel <- err
		}
	}
}

// SetDone sets the state to Done.
func (nw *NodeWriter) SetDone(client corev1.NodeInterface, lister corelisterv1.NodeLister, node string, dcAnnotation string) error {
	annos := map[string]string{
		constants.MachineConfigDaemonStateAnnotationKey: constants.MachineConfigDaemonStateDone,
		constants.CurrentMachineConfigAnnotationKey:     dcAnnotation,
	}
	respChan := make(chan error, 1)
	nw.writer <- message{
		client:          client,
		lister:          lister,
		node:            node,
		annos:           annos,
		responseChannel: respChan,
	}
	return <-respChan
}

// SetWorking Sets the state to Working.
func (nw *NodeWriter) SetWorking(client corev1.NodeInterface, lister corelisterv1.NodeLister, node string) error {
	annos := map[string]string{
		constants.MachineConfigDaemonStateAnnotationKey: constants.MachineConfigDaemonStateWorking,
	}
	respChan := make(chan error, 1)
	nw.writer <- message{
		client:          client,
		lister:          lister,
		node:            node,
		annos:           annos,
		responseChannel: respChan,
	}
	return <-respChan
}

// SetUnreconcilable Sets the state to Unreconcilable.
func (nw *NodeWriter) SetUnreconcilable(err error, client corev1.NodeInterface, lister corelisterv1.NodeLister, node string) error {
	glog.Errorf("Marking Unreconcilable due to: %v", err)
	annos := map[string]string{
		constants.MachineConfigDaemonStateAnnotationKey: constants.MachineConfigDaemonStateUnreconcilable,
	}
	respChan := make(chan error, 1)
	nw.writer <- message{
		client:          client,
		lister:          lister,
		node:            node,
		annos:           annos,
		responseChannel: respChan,
	}
	clientErr := <-respChan
	if  clientErr != nil {
		glog.Errorf("Error setting Unreconcilable annotation for node %s: %v", node, clientErr)
	}
	return clientErr
}

// SetDegraded logs the error and sets the state to Degraded.
// Returns an error if it couldn't set the annotation.
func (nw *NodeWriter) SetDegraded(err error, client corev1.NodeInterface, lister corelisterv1.NodeLister, node string) error {
	glog.Errorf("Marking Degraded due to: %v", err)
	annos := map[string]string{
		constants.MachineConfigDaemonStateAnnotationKey: constants.MachineConfigDaemonStateDegraded,
	}
	respChan := make(chan error, 1)
	nw.writer <- message{
		client:          client,
		lister:          lister,
		node:            node,
		annos:           annos,
		responseChannel: respChan,
	}
	clientErr := <-respChan
	if  clientErr != nil {
		glog.Errorf("Error setting Degraded annotation for node %s: %v", node, clientErr)
	}
	return clientErr
}

// SetSSHAccessed sets the ssh annotation to accessed
func (nw *NodeWriter) SetSSHAccessed(client corev1.NodeInterface, lister corelisterv1.NodeLister, node string) error {
	annos := map[string]string{
		machineConfigDaemonSSHAccessAnnotationKey: machineConfigDaemonSSHAccessValue,
	}
	respChan := make(chan error, 1)
	nw.writer <- message{
		client:          client,
		lister:          lister,
		node:            node,
		annos:           annos,
		responseChannel: respChan,
	}
	return <-respChan
}

// updateNodeRetry calls f to update a node object in Kubernetes.
// It will attempt to update the node by applying f to it up to DefaultBackoff
// number of times.
// f will be called each time since the node object will likely have changed if
// a retry is necessary.
func updateNodeRetry(client corev1.NodeInterface, lister corelisterv1.NodeLister, nodeName string, f func(*v1.Node)) (*v1.Node, error) {
	var node *v1.Node
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		n, err := lister.Get(nodeName)
		if err != nil {
			return err
		}
		oldNode, err := json.Marshal(n)
		if err != nil {
			return err
		}

		nodeClone := n.DeepCopy()
		f(nodeClone)

		newNode, err := json.Marshal(nodeClone)
		if err != nil {
			return err
		}

		patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldNode, newNode, v1.Node{})
		if err != nil {
			return fmt.Errorf("failed to create patch for node %q: %v", nodeName, err)
		}

		node, err = client.Patch(nodeName, types.StrategicMergePatchType, patchBytes)
		return err
	}); err != nil {
		// may be conflict if max retries were hit
		return nil, fmt.Errorf("unable to update node %q: %v", node, err)
	}
	return node, nil
}

func setNodeAnnotations(client corev1.NodeInterface, lister corelisterv1.NodeLister, nodeName string, m map[string]string) (*v1.Node, error) {
	node, err := updateNodeRetry(client, lister, nodeName, func(node *v1.Node) {
		for k, v := range m {
			node.Annotations[k] = v
		}
	})
	return node, err
}
