package plugin

import (
	"log"

	"github.com/hashicorp/terraform/helper/schema"
	"github.com/hashicorp/terraform/plugin/proto"
	"github.com/hashicorp/terraform/terraform"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/msgpack"
	context "golang.org/x/net/context"
)

type GRPCProvisionerServer struct {
	provisioner *schema.Provisioner
}

func (s *GRPCProvisionerServer) GetSchema(_ context.Context, req *proto.GetProvisionerSchema_Request) (*proto.GetProvisionerSchema_Response, error) {
	resp := &proto.GetProvisionerSchema_Response{}

	resp.Provisioner = &proto.Schema{
		Block: protoSchemaBlock(schema.InternalMap(s.provisioner.Schema).CoreConfigSchema()),
	}

	return resp, nil
}

func (s *GRPCProvisionerServer) ValidateProvisionerConfig(_ context.Context, req *proto.ValidateProvisionerConfig_Request) (*proto.ValidateProvisionerConfig_Response, error) {
	resp := &proto.ValidateProvisionerConfig_Response{}

	cfgSchema := schema.InternalMap(s.provisioner.Schema).CoreConfigSchema()

	configVal, err := msgpack.Unmarshal(req.Config.Msgpack, cfgSchema.ImpliedType())
	if err != nil {
		resp.Diagnostics = appendDiag(resp.Diagnostics, err)
		return resp, nil
	}

	config := terraform.NewResourceConfigShimmed(configVal, cfgSchema)

	warns, errs := s.provisioner.Validate(config)
	resp.Diagnostics = appendDiag(resp.Diagnostics, diagsFromWarnsErrs(warns, errs))

	return resp, nil
}

// stringMapFromValue converts a cty.Value to a map[stirng]string.
// This will panic if the val is not a cty.Map(cty.String).
func stringMapFromValue(val cty.Value) map[string]string {
	m := map[string]string{}
	if val.IsNull() || !val.IsKnown() {
		return m
	}

	for it := val.ElementIterator(); it.Next(); {
		ak, av := it.Element()
		name := ak.AsString()

		if !av.IsKnown() || av.IsNull() {
			continue
		}

		av, _ = convert.Convert(av, cty.String)
		m[name] = av.AsString()
	}

	return m
}

// uiOutput implements the terraform.UIOutput interface to adapt the grpc
// stream to the legacy Provisioner.Apply method.
type uiOutput struct {
	srv proto.Provisioner_ProvisionResourceServer
}

func (o uiOutput) Output(s string) {
	err := o.srv.Send(&proto.ProvisionResource_Response{
		Output: s,
	})
	if err != nil {
		log.Printf("[ERROR] %s", err)
	}
}

func (s *GRPCProvisionerServer) ProvisionResource(req *proto.ProvisionResource_Request, srv proto.Provisioner_ProvisionResourceServer) error {
	// We send back a diagnostics over the stream if there was a
	// provisioner-side problem.
	srvResp := &proto.ProvisionResource_Response{}

	cfgSchema := schema.InternalMap(s.provisioner.Schema).CoreConfigSchema()
	cfgVal, err := msgpack.Unmarshal(req.Config.Msgpack, cfgSchema.ImpliedType())
	if err != nil {
		srvResp.Diagnostics = appendDiag(srvResp.Diagnostics, err)
		srv.Send(srvResp)
		return nil
	}
	resourceConfig := terraform.NewResourceConfigShimmed(cfgVal, cfgSchema)

	connVal, err := msgpack.Unmarshal(req.Connection.Msgpack, cty.Map(cty.String))
	if err != nil {
		srvResp.Diagnostics = appendDiag(srvResp.Diagnostics, err)
		srv.Send(srvResp)
		return nil
	}

	conn := stringMapFromValue(connVal)

	instanceState := &terraform.InstanceState{
		Ephemeral: terraform.EphemeralState{
			ConnInfo: conn,
		},
	}

	err = s.provisioner.Apply(uiOutput{srv}, instanceState, resourceConfig)
	if err != nil {
		srvResp.Diagnostics = appendDiag(srvResp.Diagnostics, err)
		srv.Send(srvResp)
	}
	return nil
}

func (s *GRPCProvisionerServer) Stop(_ context.Context, req *proto.Stop_Request) (*proto.Stop_Response, error) {
	resp := &proto.Stop_Response{}

	err := s.provisioner.Stop()
	if err != nil {
		resp.Error = err.Error()
	}

	return resp, nil
}
