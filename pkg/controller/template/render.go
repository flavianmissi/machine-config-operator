package template

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"github.com/golang/glog"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/library-go/pkg/cloudprovider"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"

	mcfgv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	"github.com/openshift/machine-config-operator/pkg/constants"
	ctrlcommon "github.com/openshift/machine-config-operator/pkg/controller/common"
	"github.com/openshift/machine-config-operator/pkg/version"
)

// RenderConfig is wrapper around ControllerConfigSpec.
type RenderConfig struct {
	*mcfgv1.ControllerConfigSpec
	PullSecret        string
	FeatureGateAccess featuregates.FeatureGateAccess

	// no need to set this, will be automatically configured
	Constants map[string]string
}

const (
	filesDir            = "files"
	unitsDir            = "units"
	platformBase        = "_base"
	platformOnPrem      = "on-prem"
	sno                 = "sno"
	baseMasterKubeletMC = "01-master-kubelet"
	baseWorkerKubeletMC = "01-worker-kubelet"
)

// generateTemplateMachineConfigs returns MachineConfig objects from the templateDir and a config object
// expected directory structure for correctly templating machine configs: <templatedir>/<role>/<name>/<platform>/<type>/<tmpl_file>
//
// All files from platform _base are always included, and may be overridden or
// supplemented by platform-specific templates.
//
//	ex:
//	     templates/worker/00-worker/_base/units/kubelet.conf.tmpl
//	                                  /files/hostname.tmpl
//	                            /aws/units/kubelet-dropin.conf.tmpl
//	                     /01-worker-kubelet/_base/files/random.conf.tmpl
//	              /master/00-master/_base/units/kubelet.tmpl
//	                                  /files/hostname.tmpl
func generateTemplateMachineConfigs(config *RenderConfig, templateDir string) ([]*mcfgv1.MachineConfig, error) {
	infos, err := ctrlcommon.ReadDir(templateDir)
	if err != nil {
		return nil, err
	}

	cfgs := []*mcfgv1.MachineConfig{}

	for _, info := range infos {
		if !info.IsDir() {
			glog.Infof("ignoring non-directory path %q", info.Name())
			continue
		}
		role := info.Name()
		if role == "common" {
			continue
		}

		roleConfigs, err := GenerateMachineConfigsForRole(config, role, templateDir)
		if err != nil {
			return nil, fmt.Errorf("failed to create MachineConfig for role %s: %w", role, err)
		}
		cfgs = append(cfgs, roleConfigs...)
	}

	// tag all machineconfigs with the controller version
	for _, cfg := range cfgs {
		if cfg.Annotations == nil {
			cfg.Annotations = map[string]string{}
		}
		cfg.Annotations[ctrlcommon.GeneratedByControllerVersionAnnotationKey] = version.Hash
	}

	return cfgs, nil
}

// GenerateMachineConfigsForRole creates MachineConfigs for the role provided
func GenerateMachineConfigsForRole(config *RenderConfig, role, templateDir string) ([]*mcfgv1.MachineConfig, error) {
	rolePath := role
	//nolint:goconst
	if role != "worker" && role != "master" {
		// custom pools are only allowed to be worker's children
		// and can reuse the worker templates
		rolePath = "worker"
	}

	path := filepath.Join(templateDir, rolePath)
	infos, err := ctrlcommon.ReadDir(path)
	if err != nil {
		return nil, err
	}

	cfgs := []*mcfgv1.MachineConfig{}
	// This func doesn't process "common"
	// common templates are only added to 00-<role>
	// templates/<role>/{00-<role>,01-<role>-container-runtime,01-<role>-kubelet}
	var commonAdded bool
	for _, info := range infos {
		if !info.IsDir() {
			glog.Infof("ignoring non-directory path %q", info.Name())
			continue
		}
		name := info.Name()
		namePath := filepath.Join(path, name)
		nameConfig, err := generateMachineConfigForName(config, role, name, templateDir, namePath, &commonAdded)
		if err != nil {
			return nil, err
		}
		// defaulting the CGroups version to "v1"
		// (TODO) This code can be removed if not required in future releases
		if name == baseMasterKubeletMC || name == baseWorkerKubeletMC {
			updateMCwithDefaultCgroupsVersion(nameConfig)
		}
		cfgs = append(cfgs, nameConfig)
	}

	return cfgs, nil
}

// updateMCwithDefaultCgroupsVersion updates the MC kArgs with the default CGroups version config
func updateMCwithDefaultCgroupsVersion(mc *mcfgv1.MachineConfig) {
	// updating the Machine Config resource with the default cgroupv1 config
	var (
		kernelArgsv1       = []string{"systemd.unified_cgroup_hierarchy=0", "systemd.legacy_systemd_cgroup_controller=1"}
		kernelArgsv2       = []string{"systemd.unified_cgroup_hierarchy=1", "cgroup_no_v1=\"all\"", "psi=1"}
		adjustedKernelArgs []string
	)
	// avoiding the undesired kArgs from the already existing kArgs
	for _, arg := range mc.Spec.KernelArguments {
		// only append the args we want to keep, omitting the undesired
		if !ctrlcommon.InSlice(arg, kernelArgsv2) {
			adjustedKernelArgs = append(adjustedKernelArgs, arg)
		}
	}
	// appending the desired kArgs to the existing kArgs
	for _, arg := range kernelArgsv1 {
		// add the additional that aren't already there
		if !ctrlcommon.InSlice(arg, adjustedKernelArgs) {
			adjustedKernelArgs = append(adjustedKernelArgs, arg)
		}
	}
	// overwrite the KernelArguments with the adjusted KernelArgs
	mc.Spec.KernelArguments = adjustedKernelArgs
}

func platformStringFromControllerConfigSpec(ic *mcfgv1.ControllerConfigSpec) (string, error) {
	if ic.Infra == nil {
		ic.Infra = &configv1.Infrastructure{
			Status: configv1.InfrastructureStatus{},
		}
	}

	if ic.Infra.Status.PlatformStatus == nil {
		ic.Infra.Status.PlatformStatus = &configv1.PlatformStatus{
			Type: "",
		}
	}

	switch ic.Infra.Status.PlatformStatus.Type {
	case "":
		// if Platform is nil, return nil platform and an error message
		return "", fmt.Errorf("cannot generate MachineConfigs when no platformStatus.type is set")
	case platformBase:
		return "", fmt.Errorf("platform _base unsupported")
	case configv1.AWSPlatformType, configv1.AlibabaCloudPlatformType, configv1.AzurePlatformType, configv1.BareMetalPlatformType, configv1.GCPPlatformType, configv1.OpenStackPlatformType, configv1.LibvirtPlatformType, configv1.OvirtPlatformType, configv1.VSpherePlatformType, configv1.KubevirtPlatformType, configv1.PowerVSPlatformType, configv1.NonePlatformType, configv1.NutanixPlatformType:
		return strings.ToLower(string(ic.Infra.Status.PlatformStatus.Type)), nil
	default:
		// platformNone is used for a non-empty, but currently unsupported platform.
		// This allows us to incrementally roll out new platforms across the project
		// by provisioning platforms before all support is added.
		glog.Warningf("Warning: the controller config referenced an unsupported platform: %v", ic.Infra.Status.PlatformStatus.Type)

		return strings.ToLower(string(configv1.NonePlatformType)), nil
	}
}

func filterTemplates(toFilter map[string]string, path string, config *RenderConfig) error {
	walkFn := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		// empty templates signify don't create
		if info.Size() == 0 {
			delete(toFilter, info.Name())
			return nil
		}

		filedata, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read file %q: %w", path, err)
		}

		// Render the template file
		renderedData, err := renderTemplate(*config, path, filedata)
		if err != nil {
			return err
		}

		// A template may result in no data when rendered, for example if the
		// whole template is conditioned to specific values in render config.
		// The intention is there shouldn't be any resulting file or unit form
		// this template and thus we filter it here.
		// Also trim the data in case the data only consists of an extra line or space
		if len(bytes.TrimSpace(renderedData)) > 0 {
			toFilter[info.Name()] = string(renderedData)
		}

		return nil
	}

	return filepath.Walk(path, walkFn)
}

func getPaths(config *RenderConfig, platformString string) []string {
	platformBasedPaths := []string{platformBase}
	if onPremPlatform(config.Infra.Status.PlatformStatus.Type) {
		platformBasedPaths = append(platformBasedPaths, platformOnPrem)
	}

	// specific platform should be the last one in order
	// to override on-prem files in case needed
	platformBasedPaths = append(platformBasedPaths, platformString)

	// sno is specific case and it should override even specific platform files
	if config.Infra.Status.ControlPlaneTopology == configv1.SingleReplicaTopologyMode {
		platformBasedPaths = append(platformBasedPaths, sno)
	}

	return platformBasedPaths
}

func generateMachineConfigForName(config *RenderConfig, role, name, templateDir, path string, commonAdded *bool) (*mcfgv1.MachineConfig, error) {
	platformString, err := platformStringFromControllerConfigSpec(config.ControllerConfigSpec)
	if err != nil {
		return nil, err
	}

	platformDirs := []string{}
	platformBasedPaths := getPaths(config, platformString)
	if !*commonAdded {
		// Loop over templates/common which applies everywhere
		for _, dir := range platformBasedPaths {
			basePath := filepath.Join(templateDir, "common", dir)
			exists, err := existsDir(basePath)
			if err != nil {
				return nil, err
			}
			if !exists {
				continue
			}
			platformDirs = append(platformDirs, basePath)
		}
		*commonAdded = true
	}
	// And now over the target e.g. templates/master/00-master,01-master-container-runtime,01-master-kubelet
	for _, dir := range platformBasedPaths {
		platformPath := filepath.Join(path, dir)
		exists, err := existsDir(platformPath)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		platformDirs = append(platformDirs, platformPath)
	}

	files := map[string]string{}
	units := map[string]string{}
	// walk all role dirs, with later ones taking precedence
	for _, platformDir := range platformDirs {
		p := filepath.Join(platformDir, filesDir)
		exists, err := existsDir(p)
		if err != nil {
			return nil, err
		}
		if exists {
			if err := filterTemplates(files, p, config); err != nil {
				return nil, err
			}
		}

		p = filepath.Join(platformDir, unitsDir)
		exists, err = existsDir(p)
		if err != nil {
			return nil, err
		}
		if exists {
			if err := filterTemplates(units, p, config); err != nil {
				return nil, err
			}
		}
	}

	// keySortVals returns a list of values, sorted by key
	// we need the lists of files and units to have a stable ordering for the checksum
	keySortVals := func(m map[string]string) []string {
		ks := []string{}
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)

		vs := []string{}
		for _, k := range ks {
			vs = append(vs, m[k])
		}

		return vs
	}

	ignCfg, err := ctrlcommon.TranspileCoreOSConfigToIgn(keySortVals(files), keySortVals(units))
	if err != nil {
		return nil, fmt.Errorf("error transpiling CoreOS config to Ignition config: %w", err)
	}
	mcfg, err := ctrlcommon.MachineConfigFromIgnConfig(role, name, ignCfg)
	if err != nil {
		return nil, fmt.Errorf("error creating MachineConfig from Ignition config: %w", err)
	}

	// TODO(jkyros): you might think you can remove this since we override later when we merge
	// config, but resourcemerge doesn't blank this field out once it's populated
	// so if you end up on a cluster where it was ever populated in this machineconfig, it
	// will keep that last value forever once you upgrade...which is a problen now that we allow OSImageURL overrides
	// because it will look like an override when it shouldn't be. So don't take this out until you've solved that.
	// And inject the osimageurl here
	mcfg.Spec.OSImageURL = ctrlcommon.GetDefaultBaseImageContainer(config.ControllerConfigSpec)

	return mcfg, nil
}

// renderTemplate renders a template file with values from a RenderConfig
// returns the rendered file data
func renderTemplate(config RenderConfig, path string, b []byte) ([]byte, error) {
	funcs := ctrlcommon.GetTemplateFuncMap()
	funcs["skip"] = skipMissing
	funcs["cloudProvider"] = cloudProvider
	funcs["cloudConfigFlag"] = cloudConfigFlag
	funcs["onPremPlatformAPIServerInternalIP"] = onPremPlatformAPIServerInternalIP
	funcs["onPremPlatformAPIServerInternalIPs"] = onPremPlatformAPIServerInternalIPs
	funcs["onPremPlatformIngressIP"] = onPremPlatformIngressIP
	funcs["onPremPlatformIngressIPs"] = onPremPlatformIngressIPs
	funcs["onPremPlatformShortName"] = onPremPlatformShortName
	funcs["urlHost"] = urlHost
	funcs["urlPort"] = urlPort
	funcs["isOpenShiftManagedDefaultLB"] = isOpenShiftManagedDefaultLB
	tmpl, err := template.New(path).Funcs(funcs).Parse(string(b))
	if err != nil {
		return nil, fmt.Errorf("failed to parse template %s: %w", path, err)
	}

	if config.Constants == nil {
		config.Constants = constants.ConstantsByName
	}

	buf := new(bytes.Buffer)
	if err := tmpl.Execute(buf, config); err != nil {
		return nil, fmt.Errorf("failed to execute template: %w", err)
	}

	return buf.Bytes(), nil
}

var skipKeyValidate = regexp.MustCompile(`^[_a-z]\w*$`)

// Keys labelled with skip ie. {{skip "key"}}, don't need to be templated in now because at Ignition request they will be templated in with query params
func skipMissing(key string) (interface{}, error) {
	if !skipKeyValidate.MatchString(key) {
		return nil, fmt.Errorf("invalid key for skipKey")
	}

	return fmt.Sprintf("{{.%s}}", key), nil
}

func cloudProvider(cfg RenderConfig) (interface{}, error) {
	if cfg.Infra.Status.PlatformStatus != nil {
		if cfg.FeatureGateAccess == nil {
			panic("FeatureGateAccess is nil")
		}

		external, err := cloudprovider.IsCloudProviderExternal(cfg.Infra.Status.PlatformStatus, cfg.FeatureGateAccess)
		if err != nil {
			glog.Error(err)
		} else if external {
			return "external", nil
		}

		switch cfg.Infra.Status.PlatformStatus.Type {
		case configv1.AWSPlatformType, configv1.AzurePlatformType, configv1.OpenStackPlatformType, configv1.VSpherePlatformType:
			return strings.ToLower(string(cfg.Infra.Status.PlatformStatus.Type)), nil
		case configv1.GCPPlatformType:
			return "gce", nil
		default:
			return "", nil
		}
	} else {
		return "", nil
	}
}

// Process the {{cloudConfigFlag .}}
// If the CloudProviderConfig field is set and not empty, this
// returns the cloud conf flag for kubelet [1] pointing the kubelet to use
// /etc/kubernetes/cloud.conf for configuring the cloud provider for select platforms.
// By default, even if CloudProviderConfig fields is set, the kubelet will be configured to be
// used for select platforms only.
//
// [1]: https://kubernetes.io/docs/reference/command-line-tools-reference/kubelet/#options
func cloudConfigFlag(cfg RenderConfig) interface{} {
	if cfg.CloudProviderConfig == "" {
		return ""
	}

	if cfg.Infra == nil {
		cfg.Infra = &configv1.Infrastructure{
			Status: configv1.InfrastructureStatus{},
		}
	}

	if cfg.Infra.Status.PlatformStatus == nil {
		cfg.Infra.Status.PlatformStatus = &configv1.PlatformStatus{
			Type: "",
		}
	}

	if cfg.FeatureGateAccess == nil {
		panic("FeatureGateAccess is nil")
	}

	external, err := cloudprovider.IsCloudProviderExternal(cfg.Infra.Status.PlatformStatus, cfg.FeatureGateAccess)
	if err != nil {
		glog.Error(err)
	} else if external {
		return ""
	}

	flag := "--cloud-config=/etc/kubernetes/cloud.conf"
	switch cfg.Infra.Status.PlatformStatus.Type {
	case configv1.AWSPlatformType, configv1.AzurePlatformType, configv1.GCPPlatformType, configv1.OpenStackPlatformType, configv1.VSpherePlatformType:
		return flag
	default:
		return ""
	}
}

func onPremPlatformShortName(cfg RenderConfig) interface{} {
	if cfg.Infra.Status.PlatformStatus != nil {
		switch cfg.Infra.Status.PlatformStatus.Type {
		case configv1.BareMetalPlatformType:
			return "kni"
		case configv1.OvirtPlatformType:
			return "ovirt"
		case configv1.OpenStackPlatformType:
			return "openstack"
		case configv1.VSpherePlatformType:
			return "vsphere"
		case configv1.NutanixPlatformType:
			return "nutanix"
		default:
			return ""
		}
	} else {
		return ""
	}
}

// This function should be removed in 4.13 when we no longer have to worry
// about upgrades from releases that still use it.
//
//nolint:dupl
func onPremPlatformIngressIP(cfg RenderConfig) (interface{}, error) {
	if cfg.Infra.Status.PlatformStatus != nil {
		switch cfg.Infra.Status.PlatformStatus.Type {
		case configv1.BareMetalPlatformType:
			return cfg.Infra.Status.PlatformStatus.BareMetal.IngressIPs[0], nil
		case configv1.OvirtPlatformType:
			return cfg.Infra.Status.PlatformStatus.Ovirt.IngressIPs[0], nil
		case configv1.OpenStackPlatformType:
			return cfg.Infra.Status.PlatformStatus.OpenStack.IngressIPs[0], nil
		case configv1.VSpherePlatformType:
			if cfg.Infra.Status.PlatformStatus.VSphere != nil {
				if len(cfg.Infra.Status.PlatformStatus.VSphere.IngressIPs) > 0 {
					return cfg.Infra.Status.PlatformStatus.VSphere.IngressIPs[0], nil
				}
				return nil, nil
			}
			// VSphere UPI doesn't populate VSphere field. So it's not an error,
			// and there is also no data
			return nil, nil
		case configv1.NutanixPlatformType:
			return cfg.Infra.Status.PlatformStatus.Nutanix.IngressIPs[0], nil
		default:
			return nil, fmt.Errorf("invalid platform for Ingress IP")
		}
	} else {
		return nil, fmt.Errorf("")
	}
}

//nolint:dupl
func onPremPlatformIngressIPs(cfg RenderConfig) (interface{}, error) {
	if cfg.Infra.Status.PlatformStatus != nil {
		switch cfg.Infra.Status.PlatformStatus.Type {
		case configv1.BareMetalPlatformType:
			return cfg.Infra.Status.PlatformStatus.BareMetal.IngressIPs, nil
		case configv1.OvirtPlatformType:
			return cfg.Infra.Status.PlatformStatus.Ovirt.IngressIPs, nil
		case configv1.OpenStackPlatformType:
			return cfg.Infra.Status.PlatformStatus.OpenStack.IngressIPs, nil
		case configv1.VSpherePlatformType:
			if cfg.Infra.Status.PlatformStatus.VSphere != nil {
				return cfg.Infra.Status.PlatformStatus.VSphere.IngressIPs, nil
			}
			// VSphere UPI doesn't populate VSphere field. So it's not an error,
			// and there is also no data
			return []string{}, nil
		case configv1.NutanixPlatformType:
			return cfg.Infra.Status.PlatformStatus.Nutanix.IngressIPs, nil
		default:
			return nil, fmt.Errorf("invalid platform for Ingress IP")
		}
	} else {
		return nil, fmt.Errorf("")
	}
}

// This function should be removed in 4.13 when we no longer have to worry
// about upgrades from releases that still use it.
//
//nolint:dupl
func onPremPlatformAPIServerInternalIP(cfg RenderConfig) (interface{}, error) {
	if cfg.Infra.Status.PlatformStatus != nil {
		switch cfg.Infra.Status.PlatformStatus.Type {
		case configv1.BareMetalPlatformType:
			return cfg.Infra.Status.PlatformStatus.BareMetal.APIServerInternalIPs[0], nil
		case configv1.OvirtPlatformType:
			return cfg.Infra.Status.PlatformStatus.Ovirt.APIServerInternalIPs[0], nil
		case configv1.OpenStackPlatformType:
			return cfg.Infra.Status.PlatformStatus.OpenStack.APIServerInternalIPs[0], nil
		case configv1.VSpherePlatformType:
			if cfg.Infra.Status.PlatformStatus.VSphere != nil {
				if len(cfg.Infra.Status.PlatformStatus.VSphere.APIServerInternalIPs) > 0 {
					return cfg.Infra.Status.PlatformStatus.VSphere.APIServerInternalIPs[0], nil
				}
				return nil, nil
			}
			// VSphere UPI doesn't populate VSphere field. So it's not an error,
			// and there is also no data
			return nil, nil
		case configv1.NutanixPlatformType:
			return cfg.Infra.Status.PlatformStatus.Nutanix.APIServerInternalIPs[0], nil
		default:
			return nil, fmt.Errorf("invalid platform for API Server Internal IP")
		}
	} else {
		return nil, fmt.Errorf("")
	}
}

//nolint:dupl
func onPremPlatformAPIServerInternalIPs(cfg RenderConfig) (interface{}, error) {
	if cfg.Infra.Status.PlatformStatus != nil {
		switch cfg.Infra.Status.PlatformStatus.Type {
		case configv1.BareMetalPlatformType:
			return cfg.Infra.Status.PlatformStatus.BareMetal.APIServerInternalIPs, nil
		case configv1.OvirtPlatformType:
			return cfg.Infra.Status.PlatformStatus.Ovirt.APIServerInternalIPs, nil
		case configv1.OpenStackPlatformType:
			return cfg.Infra.Status.PlatformStatus.OpenStack.APIServerInternalIPs, nil
		case configv1.VSpherePlatformType:
			if cfg.Infra.Status.PlatformStatus.VSphere != nil {
				return cfg.Infra.Status.PlatformStatus.VSphere.APIServerInternalIPs, nil
			}
			// VSphere UPI doesn't populate VSphere field. So it's not an error,
			// and there is also no data
			return []string{}, nil
		case configv1.NutanixPlatformType:
			return cfg.Infra.Status.PlatformStatus.Nutanix.APIServerInternalIPs, nil
		default:
			return nil, fmt.Errorf("invalid platform for API Server Internal IP")
		}
	} else {
		return nil, fmt.Errorf("")
	}
}

// existsDir returns true if path exists and is a directory, false if the path
// does not exist, and error if there is a runtime error or the path is not a directory
func existsDir(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to open dir %q: %w", path, err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("expected template directory, %q is not a directory", path)
	}
	return true, nil
}

func onPremPlatform(platformString configv1.PlatformType) bool {
	switch platformString {
	case configv1.BareMetalPlatformType, configv1.OvirtPlatformType, configv1.OpenStackPlatformType, configv1.VSpherePlatformType, configv1.NutanixPlatformType:
		return true
	default:
		return false
	}
}

// urlHost is a template function that returns the hostname of a url (without the port)
func urlHost(u string) (interface{}, error) {
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}

	return parsed.Hostname(), nil
}

// urlPort is a template function that returns the port of a url, with defaults
// provided if necessary.
func urlPort(u string) (interface{}, error) {
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}

	port := parsed.Port()
	if port != "" {
		return port, nil
	}

	// default port
	switch parsed.Scheme {
	case "https":
		return "443", nil
	case "http":
		return "80", nil
	default:
		return "", fmt.Errorf("unknown scheme in %s", u)
	}
}

func isOpenShiftManagedDefaultLB(cfg RenderConfig) bool {
	if cfg.Infra.Status.PlatformStatus != nil {
		lbType := configv1.LoadBalancerTypeOpenShiftManagedDefault
		switch cfg.Infra.Status.PlatformStatus.Type {
		case configv1.BareMetalPlatformType:
			if cfg.Infra.Status.PlatformStatus.BareMetal != nil {
				if cfg.Infra.Status.PlatformStatus.BareMetal.LoadBalancer != nil {
					lbType = cfg.Infra.Status.PlatformStatus.BareMetal.LoadBalancer.Type
				}
				return lbType == configv1.LoadBalancerTypeOpenShiftManagedDefault
			}
		case configv1.OvirtPlatformType:
			if cfg.Infra.Status.PlatformStatus.Ovirt != nil {
				if cfg.Infra.Status.PlatformStatus.Ovirt.LoadBalancer != nil {
					lbType = cfg.Infra.Status.PlatformStatus.Ovirt.LoadBalancer.Type
				}
				return lbType == configv1.LoadBalancerTypeOpenShiftManagedDefault
			}
		case configv1.OpenStackPlatformType:
			if cfg.Infra.Status.PlatformStatus.OpenStack != nil {
				if cfg.Infra.Status.PlatformStatus.OpenStack.LoadBalancer != nil {
					lbType = cfg.Infra.Status.PlatformStatus.OpenStack.LoadBalancer.Type
				}
				return lbType == configv1.LoadBalancerTypeOpenShiftManagedDefault
			}
		case configv1.VSpherePlatformType:
			if cfg.Infra.Status.PlatformStatus.VSphere != nil {
				// vSphere allows to use a user managed load balancer by not setting the VIPs in PlatformStatus.
				// We will maintain backward compatibility by checking if the VIPs are not set, we will
				// not deploy HAproxy, Keepalived and CoreDNS.
				if len(cfg.Infra.Status.PlatformStatus.VSphere.APIServerInternalIPs) == 0 {
					return false
				}
				if cfg.Infra.Status.PlatformStatus.VSphere.LoadBalancer != nil {
					lbType = cfg.Infra.Status.PlatformStatus.VSphere.LoadBalancer.Type
				}
				return lbType == configv1.LoadBalancerTypeOpenShiftManagedDefault
			}
			glog.Info("VSphere UPI doesn't populate VSphere PlatformStatus field. In that case we should return false")
			return false
		case configv1.NutanixPlatformType:
			if cfg.Infra.Status.PlatformStatus.Nutanix != nil {
				if cfg.Infra.Status.PlatformStatus.Nutanix.LoadBalancer != nil {
					lbType = cfg.Infra.Status.PlatformStatus.Nutanix.LoadBalancer.Type
				}
				return lbType == configv1.LoadBalancerTypeOpenShiftManagedDefault
			}
		default:
			// If a new on-prem platform is newly supported, the default value of LoadBalancerType is internal.
			return true
		}
	}
	return false
}
