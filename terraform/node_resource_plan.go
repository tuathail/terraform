package terraform

import (
	"log"

	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/dag"
	"github.com/hashicorp/terraform/tfdiags"
)

// nodeExpandPlannableResource handles the first layer of resource
// expansion.  We need this extra layer so DynamicExpand is called twice for
// the resource, the first to expand the Resource for each module instance, and
// the second to expand each ResourceInstnace for the expanded Resources.
//
// Even though the Expander can handle this recursive expansion, the
// EvalWriteState nodes need to be expanded and Evaluated first, and our
// current graph doesn't allow evaluation within DynamicExpand, and doesn't
// call it recursively.
type nodeExpandPlannableResource struct {
	*NodeAbstractResource

	// TODO: can we eliminate the need for this flag and combine this with the
	// apply expander?
	// ForceCreateBeforeDestroy might be set via our GraphNodeDestroyerCBD
	// during graph construction, if dependencies require us to force this
	// on regardless of what the configuration says.
	ForceCreateBeforeDestroy *bool
}

var (
	_ GraphNodeDestroyerCBD         = (*nodeExpandPlannableResource)(nil)
	_ GraphNodeDynamicExpandable    = (*nodeExpandPlannableResource)(nil)
	_ GraphNodeReferenceable        = (*nodeExpandPlannableResource)(nil)
	_ GraphNodeReferencer           = (*nodeExpandPlannableResource)(nil)
	_ GraphNodeConfigResource       = (*nodeExpandPlannableResource)(nil)
	_ GraphNodeAttachResourceConfig = (*nodeExpandPlannableResource)(nil)
)

// GraphNodeDestroyerCBD
func (n *nodeExpandPlannableResource) CreateBeforeDestroy() bool {
	if n.ForceCreateBeforeDestroy != nil {
		return *n.ForceCreateBeforeDestroy
	}

	// If we have no config, we just assume no
	if n.Config == nil || n.Config.Managed == nil {
		return false
	}

	return n.Config.Managed.CreateBeforeDestroy
}

// GraphNodeDestroyerCBD
func (n *nodeExpandPlannableResource) ModifyCreateBeforeDestroy(v bool) error {
	n.ForceCreateBeforeDestroy = &v
	return nil
}

func (n *nodeExpandPlannableResource) DynamicExpand(ctx EvalContext) (*Graph, error) {
	var g Graph

	expander := ctx.InstanceExpander()
	for _, module := range expander.ExpandModule(n.Addr.Module) {
		g.Add(&NodePlannableResource{
			NodeAbstractResource:     n.NodeAbstractResource,
			Addr:                     n.Addr.Resource.Absolute(module),
			ForceCreateBeforeDestroy: n.ForceCreateBeforeDestroy,
		})
	}

	return &g, nil
}

// NodePlannableResource represents a resource that is "plannable":
// it is ready to be planned in order to create a diff.
type NodePlannableResource struct {
	*NodeAbstractResource

	Addr addrs.AbsResource

	// ForceCreateBeforeDestroy might be set via our GraphNodeDestroyerCBD
	// during graph construction, if dependencies require us to force this
	// on regardless of what the configuration says.
	ForceCreateBeforeDestroy *bool
}

var (
	_ GraphNodeModuleInstance       = (*NodePlannableResource)(nil)
	_ GraphNodeDestroyerCBD         = (*NodePlannableResource)(nil)
	_ GraphNodeDynamicExpandable    = (*NodePlannableResource)(nil)
	_ GraphNodeReferenceable        = (*NodePlannableResource)(nil)
	_ GraphNodeReferencer           = (*NodePlannableResource)(nil)
	_ GraphNodeConfigResource       = (*NodePlannableResource)(nil)
	_ GraphNodeAttachResourceConfig = (*NodePlannableResource)(nil)
)

func (n *NodePlannableResource) Path() addrs.ModuleInstance {
	return n.Addr.Module
}

// GraphNodeModuleInstance
func (n *NodePlannableResource) ModuleInstance() addrs.ModuleInstance {
	return n.Addr.Module
}

// GraphNodeEvalable
func (n *NodePlannableResource) EvalTree() EvalNode {
	if n.Config == nil {
		// Nothing to do, then.
		log.Printf("[TRACE] NodeApplyableResource: no configuration present for %s", n.Name())
		return &EvalNoop{}
	}

	// this ensures we can reference the resource even if the count is 0
	return &EvalWriteResourceState{
		Addr:         n.Addr,
		Config:       n.Config,
		ProviderAddr: n.ResolvedProvider,
	}
}

// GraphNodeDestroyerCBD
func (n *NodePlannableResource) CreateBeforeDestroy() bool {
	if n.ForceCreateBeforeDestroy != nil {
		return *n.ForceCreateBeforeDestroy
	}

	// If we have no config, we just assume no
	if n.Config == nil || n.Config.Managed == nil {
		return false
	}

	return n.Config.Managed.CreateBeforeDestroy
}

// GraphNodeDestroyerCBD
func (n *NodePlannableResource) ModifyCreateBeforeDestroy(v bool) error {
	n.ForceCreateBeforeDestroy = &v
	return nil
}

// GraphNodeDynamicExpandable
func (n *NodePlannableResource) DynamicExpand(ctx EvalContext) (*Graph, error) {
	var diags tfdiags.Diagnostics

	// Our instance expander should already have been informed about the
	// expansion of this resource and of all of its containing modules, so
	// it can tell us which instance addresses we need to process.
	expander := ctx.InstanceExpander()
	instanceAddrs := expander.ExpandResource(n.ResourceAddr().Absolute(ctx.Path()))

	// We need to potentially rename an instance address in the state
	// if we're transitioning whether "count" is set at all.
	//
	// FIXME: We're re-evaluating count here, even though the InstanceExpander
	// has already dealt with our expansion above, because we need it to
	// call fixResourceCountSetTransition; the expander API and that function
	// are not compatible yet.
	count, countDiags := evaluateResourceCountExpression(n.Config.Count, ctx)
	diags = diags.Append(countDiags)
	if countDiags.HasErrors() {
		return nil, diags.Err()
	}
	fixResourceCountSetTransition(ctx, n.Addr.Config(), count != -1)

	// Our graph transformers require access to the full state, so we'll
	// temporarily lock it while we work on this.
	state := ctx.State().Lock()
	defer ctx.State().Unlock()

	// The concrete resource factory we'll use
	concreteResource := func(a *NodeAbstractResourceInstance) dag.Vertex {
		// Add the config and state since we don't do that via transforms
		a.Config = n.Config
		a.ResolvedProvider = n.ResolvedProvider
		a.Schema = n.Schema
		a.ProvisionerSchemas = n.ProvisionerSchemas
		a.ProviderMetas = n.ProviderMetas

		return &NodePlannableResourceInstance{
			NodeAbstractResourceInstance: a,

			// By the time we're walking, we've figured out whether we need
			// to force on CreateBeforeDestroy due to dependencies on other
			// nodes that have it.
			ForceCreateBeforeDestroy: n.CreateBeforeDestroy(),
		}
	}

	// The concrete resource factory we'll use for orphans
	concreteResourceOrphan := func(a *NodeAbstractResourceInstance) dag.Vertex {
		// Add the config and state since we don't do that via transforms
		a.Config = n.Config
		a.ResolvedProvider = n.ResolvedProvider
		a.Schema = n.Schema
		a.ProvisionerSchemas = n.ProvisionerSchemas
		a.ProviderMetas = n.ProviderMetas

		return &NodePlannableResourceInstanceOrphan{
			NodeAbstractResourceInstance: a,
		}
	}

	// Start creating the steps
	steps := []GraphTransformer{
		// Expand the count or for_each (if present)
		&ResourceCountTransformer{
			Concrete:      concreteResource,
			Schema:        n.Schema,
			Addr:          n.ResourceAddr(),
			InstanceAddrs: instanceAddrs,
		},

		// Add the count/for_each orphans
		&OrphanResourceCountTransformer{
			Concrete:      concreteResourceOrphan,
			Addr:          n.ResourceAddr(),
			InstanceAddrs: instanceAddrs,
			State:         state,
		},

		// Attach the state
		&AttachStateTransformer{State: state},

		// Targeting
		&TargetsTransformer{Targets: n.Targets},

		// Connect references so ordering is correct
		&ReferenceTransformer{},

		// Make sure there is a single root
		&RootTransformer{},
	}

	// Build the graph
	b := &BasicGraphBuilder{
		Steps:    steps,
		Validate: true,
		Name:     "NodePlannableResource",
	}
	graph, diags := b.Build(ctx.Path())
	return graph, diags.ErrWithWarnings()
}
