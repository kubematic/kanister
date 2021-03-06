package kanctl

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/ghodss/yaml"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	crv1alpha1 "github.com/kanisterio/kanister/pkg/apis/cr/v1alpha1"
	"github.com/kanisterio/kanister/pkg/client/clientset/versioned"
	"github.com/kanisterio/kanister/pkg/param"
)

const (
	actionFlagName        = "action"
	blueprintFlagName     = "blueprint"
	configMapsFlagName    = "config-maps"
	deploymentFlagName    = "deployment"
	optionsFlagName       = "options"
	profileFlagName       = "profile"
	pvcFlagName           = "pvc"
	secretsFlagName       = "secrets"
	statefulSetFlagName   = "statefulset"
	sourceFlagName        = "from"
	selectorFlagName      = "selector"
	selectorKindFlag      = "kind"
	selectorNamespaceFlag = "selector-namespace"
)

type performParams struct {
	namespace  string
	actionName string
	parentName string
	blueprint  string
	dryRun     bool
	objects    []crv1alpha1.ObjectReference
	options    map[string]string
	profile    *crv1alpha1.ObjectReference
	secrets    map[string]crv1alpha1.ObjectReference
	configMaps map[string]crv1alpha1.ObjectReference
}

func newActionSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "actionset",
		Short: "Create a new ActionSet or override a <parent> ActionSet",
		Args:  cobra.ExactArgs(0),
		RunE: func(c *cobra.Command, args []string) error {
			return initializeAndPerform(c, args)
		},
	}
	cmd.Flags().StringP(sourceFlagName, "f", "", "specify name of the action set")

	cmd.Flags().StringP(actionFlagName, "a", "", "action for the action set (required if creating a new action set)")
	cmd.Flags().StringP(blueprintFlagName, "b", "", "blueprint for the action set (required if creating a new action set)")
	cmd.Flags().StringSliceP(configMapsFlagName, "c", []string{}, "config maps for the action set, comma separated ref=namespace/name pairs (eg: --config-maps ref1=namespace1/name1,ref2=namespace2/name2)")
	cmd.Flags().StringSliceP(deploymentFlagName, "d", []string{}, "deployment for the action set, comma separated namespace/name pairs (eg: --deployment namespace1/name1,namespace2/name2)")
	cmd.Flags().StringSliceP(optionsFlagName, "o", []string{}, "specify options for the action set, comma separated key=value pairs (eg: --options key1=value1,key2=value2)")
	cmd.Flags().StringP(profileFlagName, "p", "", "profile for the action set")
	cmd.Flags().StringSliceP(pvcFlagName, "v", []string{}, "pvc for the action set, comma separated namespace/name pairs (eg: --pvc namespace1/name1,namespace2/name2)")
	cmd.Flags().StringSliceP(secretsFlagName, "s", []string{}, "secrets for the action set, comma separated ref=namespace/name pairs (eg: --secrets ref1=namespace1/name1,ref2=namespace2/name2)")
	cmd.Flags().StringSliceP(statefulSetFlagName, "t", []string{}, "statefulset for the action set, comma separated namespace/name pairs (eg: --statefulset namespace1/name1,namespace2/name2)")
	cmd.Flags().StringP(selectorFlagName, "l", "", "k8s selector for objects")
	cmd.Flags().StringP(selectorKindFlag, "k", "all", "resource kind to apply selector on. Used along with the selector specified using --selector/-l")
	cmd.Flags().String(selectorNamespaceFlag, "", "namespace to apply selector on. Used along with the selector specified using --selector/-l")
	return cmd
}

func initializeAndPerform(cmd *cobra.Command, args []string) error {
	cli, crCli, err := initializeClients()
	if err != nil {
		return err
	}
	params, err := extractPerformParams(cmd, args, cli)
	if err != nil {
		return err
	}
	cmd.SilenceUsage = true
	ctx := context.Background()
	valFlag, _ := cmd.Flags().GetBool(skipValidationFlag)
	if !valFlag {
		err = verifyParams(ctx, params, cli, crCli)
		if err != nil {
			return err
		}
	}
	return perform(ctx, crCli, params)
}

func perform(ctx context.Context, crCli versioned.Interface, params *performParams) error {
	var as *crv1alpha1.ActionSet
	var err error

	switch {
	case params.parentName != "":
		pas, err := crCli.CrV1alpha1().ActionSets(params.namespace).Get(params.parentName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		as, err = childActionSet(pas, params)
	case len(params.objects) > 0:
		as, err = newActionSet(params)
	default:
		return errors.New("no objects found to perform action set. Please pass a valid parent action set and/or selector")
	}
	if err != nil {
		return err
	}
	if params.dryRun {
		return printActionSet(as)
	}
	return createActionSet(ctx, crCli, params.namespace, as)
}

func newActionSet(params *performParams) (*crv1alpha1.ActionSet, error) {
	if params.actionName == "" {
		return nil, errors.New("action required to create new action set")
	}
	if params.blueprint == "" {
		return nil, errors.New("blueprint required to create new action set")
	}
	actions := make([]crv1alpha1.ActionSpec, 0, len(params.objects))
	for _, obj := range params.objects {
		actions = append(actions, crv1alpha1.ActionSpec{
			Name:       params.actionName,
			Blueprint:  params.blueprint,
			Object:     obj,
			Secrets:    params.secrets,
			ConfigMaps: params.configMaps,
			Profile:    params.profile,
			Options:    params.options,
		})
	}
	name := fmt.Sprintf("%s-", params.actionName)
	return &crv1alpha1.ActionSet{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: name,
		},
		Spec: &crv1alpha1.ActionSetSpec{
			Actions: actions,
		},
	}, nil
}

func childActionSet(parent *crv1alpha1.ActionSet, params *performParams) (*crv1alpha1.ActionSet, error) {
	if parent.Status == nil || parent.Status.State != crv1alpha1.StateComplete {
		return nil, errors.Errorf("Request parent ActionSet %s has not been executed", parent.GetName())
	}

	actions := make([]crv1alpha1.ActionSpec, 0, len(parent.Status.Actions)*max(1, len(params.objects)))
	for aidx, pa := range parent.Status.Actions {
		as := crv1alpha1.ActionSpec{
			Name:       parent.Spec.Actions[aidx].Name,
			Blueprint:  pa.Blueprint,
			Object:     pa.Object,
			Artifacts:  pa.Artifacts,
			Secrets:    parent.Spec.Actions[aidx].Secrets,
			ConfigMaps: parent.Spec.Actions[aidx].ConfigMaps,
			Profile:    parent.Spec.Actions[aidx].Profile,
			Options:    mergeOptions(params.options, parent.Spec.Actions[aidx].Options),
		}
		// Apply overrides
		if params.actionName != "" {
			as.Name = params.actionName
		}
		if params.blueprint != "" {
			as.Blueprint = params.blueprint
		}
		if len(params.secrets) > 0 {
			as.Secrets = params.secrets
		}
		if len(params.configMaps) > 0 {
			as.ConfigMaps = params.configMaps
		}
		if params.profile != nil {
			as.Profile = params.profile
		}
		if len(params.objects) > 0 {
			for _, obj := range params.objects {
				asCopy := as.DeepCopy()
				asCopy.Object = obj

				actions = append(actions, *asCopy)
			}
		} else {
			actions = append(actions, as)
		}
	}
	return &crv1alpha1.ActionSet{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: func() string {
				if params.actionName != "" {
					return fmt.Sprintf("%s-%s-", params.actionName, parent.GetName())
				}
				return fmt.Sprintf("%s-", parent.GetName())
			}(),
		},
		Spec: &crv1alpha1.ActionSetSpec{
			Actions: actions,
		},
	}, nil
}

func createActionSet(ctx context.Context, crCli versioned.Interface, namespace string, as *crv1alpha1.ActionSet) error {
	as, err := crCli.CrV1alpha1().ActionSets(namespace).Create(as)
	if err == nil {
		fmt.Printf("actionset %s created\n", as.Name)
	}
	return err
}

func printActionSet(as *crv1alpha1.ActionSet) error {
	as.TypeMeta = metav1.TypeMeta{
		Kind:       crv1alpha1.ActionSetResource.Kind,
		APIVersion: crv1alpha1.SchemeGroupVersion.String(),
	}
	asYAML, err := yaml.Marshal(as)
	if err != nil {
		return errors.New("could not convert generated action set to YAML")
	}
	fmt.Printf(string(asYAML))
	return nil
}

func extractPerformParams(cmd *cobra.Command, args []string, cli kubernetes.Interface) (*performParams, error) {
	if len(args) != 0 {
		return nil, newArgsLengthError("expected 0 arguments. got %#v", args)
	}
	ns, err := resolveNamespace(cmd)
	if err != nil {
		return nil, err
	}
	actionName, _ := cmd.Flags().GetString(actionFlagName)
	parentName, _ := cmd.Flags().GetString(sourceFlagName)
	blueprint, _ := cmd.Flags().GetString(blueprintFlagName)
	dryRun, _ := cmd.Flags().GetBool(dryRunFlag)
	profile := parseProfile(cmd, ns)
	cms, err := parseConfigMaps(cmd)
	if err != nil {
		return nil, err
	}
	objects, err := parseObjects(cmd, cli)
	if err != nil {
		return nil, err
	}
	options, err := parseOptions(cmd)
	if err != nil {
		return nil, err
	}
	secrets, err := parseSecrets(cmd)
	if err != nil {
		return nil, err
	}
	return &performParams{
		namespace:  ns,
		actionName: actionName,
		parentName: parentName,
		blueprint:  blueprint,
		dryRun:     dryRun,
		objects:    objects,
		options:    options,
		secrets:    secrets,
		configMaps: cms,
		profile:    profile,
	}, nil
}

func parseConfigMaps(cmd *cobra.Command) (map[string]crv1alpha1.ObjectReference, error) {
	configMapsFromCmd, _ := cmd.Flags().GetStringSlice(configMapsFlagName)
	cms, err := parseReferences(configMapsFromCmd)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse config maps")
	}
	return cms, nil
}

func parseProfile(cmd *cobra.Command, ns string) *crv1alpha1.ObjectReference {
	profileName, _ := cmd.Flags().GetString(profileFlagName)
	if profileName == "" {
		return nil
	}
	return &crv1alpha1.ObjectReference{
		Name:      profileName,
		Namespace: ns,
	}
}

func parseSecrets(cmd *cobra.Command) (map[string]crv1alpha1.ObjectReference, error) {
	secretsFromCmd, _ := cmd.Flags().GetStringSlice(secretsFlagName)
	secrets, err := parseReferences(secretsFromCmd)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse secrets")
	}
	return secrets, nil
}

func parseObjects(cmd *cobra.Command, cli kubernetes.Interface) ([]crv1alpha1.ObjectReference, error) {
	var objects []crv1alpha1.ObjectReference
	objs := make(map[string][]string)

	deployments, _ := cmd.Flags().GetStringSlice(deploymentFlagName)
	statefulSets, _ := cmd.Flags().GetStringSlice(statefulSetFlagName)
	pvcs, _ := cmd.Flags().GetStringSlice(pvcFlagName)
	objs[param.DeploymentKind] = deployments
	objs[param.StatefulSetKind] = statefulSets
	objs[param.PVCKind] = pvcs

	parsed := make(map[string]bool)
	fromCmd, err := parseObjectsFromCmd(objs, parsed)
	if err != nil {
		return nil, err
	}
	objects = append(objects, fromCmd...)

	selectorString, _ := cmd.Flags().GetString(selectorFlagName)
	if selectorString != "" {
		// parse selector before making calls to K8s
		selector, err := labels.Parse(selectorString)
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse selector")
		}
		kind, _ := cmd.Flags().GetString(selectorKindFlag)
		sns, _ := cmd.Flags().GetString(selectorNamespaceFlag)
		fromSelector, err := parseObjectsFromSelector(selector.String(), kind, sns, cli, parsed)
		if err != nil {
			return nil, err
		}
		objects = append(objects, fromSelector...)
	}
	return objects, nil
}

func parseObjectsFromCmd(objs map[string][]string, parsed map[string]bool) ([]crv1alpha1.ObjectReference, error) {
	var objects []crv1alpha1.ObjectReference
	for kind, resources := range objs {
		for _, resource := range resources {
			namespace, name, err := parseName(resource)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to parse %s", kind)
			}
			obj := fmt.Sprintf("%s=%s/%s", kind, namespace, name)
			if _, ok := parsed[obj]; ok || obj == "" {
				continue
			}
			parsed[obj] = true
			switch strings.ToLower(kind) {
			case param.DeploymentKind:
				objects = append(objects, crv1alpha1.ObjectReference{Kind: param.DeploymentKind, Namespace: namespace, Name: name})
			case param.StatefulSetKind:
				objects = append(objects, crv1alpha1.ObjectReference{Kind: param.StatefulSetKind, Namespace: namespace, Name: name})
			case param.PVCKind:
				objects = append(objects, crv1alpha1.ObjectReference{Kind: param.PVCKind, Namespace: namespace, Name: name})
			default:
				return nil, errors.Errorf("unsupported or unknown object kind '%s'. Supported %s, %s and %s", kind, param.DeploymentKind, param.StatefulSetKind, param.PVCKind)
			}
		}
	}
	return objects, nil
}

func parseObjectsFromSelector(selector, kind, sns string, cli kubernetes.Interface, parsed map[string]bool) ([]crv1alpha1.ObjectReference, error) {
	var objects []crv1alpha1.ObjectReference
	appendObj := func(kind, namespace, name string) {
		r := fmt.Sprintf("%s=%s/%s", kind, namespace, name)
		if _, ok := parsed[r]; !ok {
			objects = append(objects, crv1alpha1.ObjectReference{Kind: kind, Namespace: namespace, Name: name})
			parsed[r] = true
		}
	}
	switch kind {
	case "all":
		fallthrough
	case param.DeploymentKind:
		dpts, err := cli.AppsV1().Deployments(sns).List(metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return nil, errors.Errorf("failed to get deployments using selector '%s' in namespace '%s'", selector, sns)
		}
		for _, d := range dpts.Items {
			appendObj(param.DeploymentKind, d.Namespace, d.Name)
		}
		if kind != "all" {
			break
		}
		fallthrough
	case param.StatefulSetKind:
		ss, err := cli.AppsV1().StatefulSets(sns).List(metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return nil, errors.Errorf("failed to get statefulsets using selector '%s' in namespace '%s'", selector, sns)
		}
		for _, s := range ss.Items {
			appendObj(param.StatefulSetKind, s.Namespace, s.Name)
		}
		if kind != "all" {
			break
		}
		fallthrough
	case param.PVCKind:
		pvcs, err := cli.CoreV1().PersistentVolumeClaims(sns).List(metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return nil, errors.Errorf("failed to get pvcs using selector '%s' in namespace '%s'", selector, sns)
		}
		for _, pvc := range pvcs.Items {
			appendObj(param.PVCKind, pvc.Namespace, pvc.Name)
		}
	default:
		return nil, errors.Errorf("unsupported or unknown object kind '%s'. Supported %s, %s and %s", kind, param.DeploymentKind, param.StatefulSetKind, param.PVCKind)
	}
	return objects, nil
}

func parseOptions(cmd *cobra.Command) (map[string]string, error) {
	optionsFromCmd, _ := cmd.Flags().GetStringSlice(optionsFlagName)
	options := make(map[string]string)

	for _, kv := range optionsFromCmd {
		if kv == "" {
			continue
		}
		// Cobra takes care of trimming spaces
		kvPair := strings.Split(kv, "=")
		if len(kvPair) != 2 {
			return nil, errors.Errorf("Expected options as key=value pairs. Got %s", kv)
		}
		options[kvPair[0]] = kvPair[1]
	}
	return options, nil
}

func mergeOptions(src map[string]string, dst map[string]string) map[string]string {
	final := make(map[string]string, len(src)+len(dst))
	for k, v := range dst {
		final[k] = v
	}
	// Override default options and set additional ones
	for k, v := range src {
		final[k] = v
	}
	return final
}

func parseReferences(references []string) (map[string]crv1alpha1.ObjectReference, error) {
	m := make(map[string]crv1alpha1.ObjectReference)
	parsed := make(map[string]bool)

	for _, r := range references {
		if _, ok := parsed[r]; ok || r == "" {
			continue
		}
		parsed[r] = true
		ref, namespace, name, err := parseReference(r)
		if err != nil {
			return nil, err
		}
		m[ref] = crv1alpha1.ObjectReference{
			Name:      name,
			Namespace: namespace,
		}
	}
	return m, nil
}

func parseReference(r string) (ref, namespace, name string, err error) {
	reg := regexp.MustCompile(`([\w-.]+)=([\w-.]+)/([\w-.]+)`)
	matches := reg.FindStringSubmatch(r)
	if len(matches) != 4 {
		return "", "", "", errors.Errorf("Expected ref=namespace/name. Got %s", r)
	}
	return matches[1], matches[2], matches[3], nil
}

func parseName(r string) (namespace, name string, err error) {
	reg := regexp.MustCompile(`([\w-.]+)/([\w-.]+)`)
	m := reg.FindStringSubmatch(r)
	if len(m) != 3 {
		return "", "", errors.Errorf("Expected namespace/name. Got %s", r)
	}
	return m[1], m[2], nil
}

func verifyParams(ctx context.Context, p *performParams, cli kubernetes.Interface, crCli versioned.Interface) error {
	const notFoundTmpl = "Please make sure '%s' with name '%s' exists in namespace '%s'"
	msgs := make(chan error)
	wg := sync.WaitGroup{}
	wg.Add(5)

	// Blueprint
	go func() {
		defer wg.Done()
		if p.blueprint != "" {
			_, err := crCli.CrV1alpha1().Blueprints(p.namespace).Get(p.blueprint, metav1.GetOptions{})
			if err != nil {
				msgs <- errors.Wrapf(err, notFoundTmpl, "blueprint", p.blueprint, p.namespace)
			}
		}
	}()

	// Profile
	go func() {
		defer wg.Done()
		if p.profile != nil {
			_, err := crCli.CrV1alpha1().Profiles(p.profile.Namespace).Get(p.profile.Name, metav1.GetOptions{})
			if err != nil {
				msgs <- errors.Wrapf(err, notFoundTmpl, "profile", p.profile.Name, p.profile.Namespace)
			}
		}
	}()

	// Objects
	go func() {
		defer wg.Done()
		var err error
		for _, obj := range p.objects {
			switch obj.Kind {
			case param.DeploymentKind:
				_, err = cli.AppsV1().Deployments(obj.Namespace).Get(obj.Name, metav1.GetOptions{})
			case param.StatefulSetKind:
				_, err = cli.AppsV1().StatefulSets(obj.Namespace).Get(obj.Name, metav1.GetOptions{})
			case param.PVCKind:
				_, err = cli.CoreV1().PersistentVolumeClaims(obj.Namespace).Get(obj.Name, metav1.GetOptions{})
			}
			if err != nil {
				msgs <- errors.Wrapf(err, notFoundTmpl, obj.Kind, obj.Name, obj.Namespace)
			}
		}
	}()

	// ConfigMaps
	go func() {
		defer wg.Done()
		for _, cm := range p.configMaps {
			_, err := cli.CoreV1().ConfigMaps(cm.Namespace).Get(cm.Name, metav1.GetOptions{})
			if err != nil {
				msgs <- errors.Wrapf(err, notFoundTmpl, "config map", cm.Name, cm.Namespace)
			}
		}
	}()

	// Secrets
	go func() {
		defer wg.Done()
		for _, secret := range p.secrets {
			_, err := cli.CoreV1().Secrets(secret.Namespace).Get(secret.Name, metav1.GetOptions{})
			if err != nil {
				msgs <- errors.Wrapf(err, notFoundTmpl, "secret", secret.Name, secret.Namespace)
			}
		}
	}()

	go func() {
		wg.Wait()
		close(msgs)
	}()

	vFail := false
	for msg := range msgs {
		vFail = true
		fmt.Println(msg)
	}

	if vFail {
		return errors.Errorf("resource verification failed")
	}
	return nil
}

func max(x, y int) int {
	if x > y {
		return x
	}
	return y
}
