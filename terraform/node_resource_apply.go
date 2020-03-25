package terraform

import (
	"log"

	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/dag"
	"github.com/hashicorp/terraform/lang"
)

// NodeApplyableResource represents a resource that is "applyable":
// it may need to have its record in the state adjusted to match configuration.
//
// Unlike in the plan walk, this resource node does not DynamicExpand. Instead,
// it should be inserted into the same graph as any instances of the nodes
// with dependency edges ensuring that the resource is evaluated before any
// of its instances, which will turn ensure that the whole-resource record
// in the state is suitably prepared to receive any updates to instances.
type NodeApplyableResource struct {
	*NodeAbstractResource
}

var (
	_ GraphNodeConfigResource       = (*NodeApplyableResource)(nil)
	_ GraphNodeEvalable             = (*NodeApplyableResource)(nil)
	_ GraphNodeProviderConsumer     = (*NodeApplyableResource)(nil)
	_ GraphNodeAttachResourceConfig = (*NodeApplyableResource)(nil)
	_ GraphNodeReferencer           = (*NodeApplyableResource)(nil)
)

func (n *NodeApplyableResource) Name() string {
	return n.NodeAbstractResource.Name() + " (prepare state)"
}

func (n *NodeApplyableResource) References() []*addrs.Reference {
	if n.Config == nil {
		log.Printf("[WARN] NodeApplyableResource %q: no configuration, so can't determine References", dag.VertexName(n))
		return nil
	}

	var result []*addrs.Reference

	// Since this node type only updates resource-level metadata, we only
	// need to worry about the parts of the configuration that affect
	// our "each mode": the count and for_each meta-arguments.
	refs, _ := lang.ReferencesInExpr(n.Config.Count)
	result = append(result, refs...)
	refs, _ = lang.ReferencesInExpr(n.Config.ForEach)
	result = append(result, refs...)

	return result
}

// GraphNodeEvalable
func (n *NodeApplyableResource) EvalTree() EvalNode {
	if n.Config == nil {
		// Nothing to do, then.
		log.Printf("[TRACE] NodeApplyableResource: no configuration present for %s", n.Name())
		return &EvalNoop{}
	}

	return &EvalWriteResourceState{
		Addr:         n.Addr.Resource.Absolute(n.Addr.Module.UnkeyedInstanceShim()),
		Config:       n.Config,
		ProviderAddr: n.ResolvedProvider,
	}
}
