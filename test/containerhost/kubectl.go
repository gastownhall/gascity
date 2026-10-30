package containerhost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/util/jsonpath"
)

// Kubectl runs one kubectl invocation against the emulated host named by
// RootEnv: an API server and kubelet for Pods only. A pod is one sandbox
// (container) whose init containers run to completion in order before its
// containers start, with restartPolicy Never semantics; volumes are emptyDir
// or secret (an empty directory). Objects are the real core/v1 types and
// output goes through client-go's jsonpath, so -o jsonpath/json read exactly
// what kubectl prints. Anything else fails closed.
func Kubectl(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	h, err := FromEnv()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kubectl (emulated): %v\n", err)
		return 1
	}
	return h.Kubectl(args, stdin, stdout, stderr)
}

// Kubectl runs one kubectl invocation against h.
func (h *Host) Kubectl(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	k := kubectlCLI{host: h, stdin: stdin, stdout: stdout, stderr: stderr, namespace: "default"}
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		name, value, hasValue := strings.Cut(a, "=")
		switch name {
		case "-n", "--namespace", "--context":
			if !hasValue {
				if i+1 >= len(args) {
					return k.fail(1, "error: flag needs an argument: %s", name)
				}
				i++
				value = args[i]
			}
			if name != "--context" {
				k.namespace = value
			}
			continue
		}
		break
	}
	if i >= len(args) {
		return k.fail(1, "kubectl (emulated): no command")
	}
	verb, rest := args[i], args[i+1:]
	switch verb {
	case "get":
		return k.get(rest)
	case "apply":
		return k.apply(rest)
	case "delete":
		return k.delete(rest)
	case "wait":
		return k.wait(rest)
	case "exec":
		return k.exec(rest)
	case "cp":
		return k.cp(rest)
	}
	return k.fail(1, "kubectl (emulated): unsupported command %q", args)
}

type kubectlCLI struct {
	host      *Host
	namespace string
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
}

func (k kubectlCLI) fail(code int, format string, a ...any) int {
	_, _ = fmt.Fprintf(k.stderr, format+"\n", a...)
	return code
}

// podRecord is the stored state of one pod.
type podRecord struct {
	Pod corev1.Pod `json:"pod"`
	// Sandbox is the container all of the pod's containers run in; empty
	// while an image is missing (the pod stays Pending, as with an image
	// that cannot be pulled).
	Sandbox     string `json:"sandbox"`
	NextInit    int    `json:"next_init"`
	MainStarted bool   `json:"main_started"`
	// Failure is a container start error that failed the pod.
	Failure string `json:"failure"`
}

func (k kubectlCLI) podDir() string {
	return filepath.Join(k.host.Root, "k8s", k.namespace)
}

func (k kubectlCLI) podPath(name string) string {
	return filepath.Join(k.podDir(), name+".json")
}

func (k kubectlCLI) load(name string) (*podRecord, error) {
	var rec podRecord
	if err := readJSON(k.podPath(name), &rec); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errPodNotFound
		}
		return nil, err
	}
	return &rec, nil
}

var errPodNotFound = errors.New("not found")

func (k kubectlCLI) notFound(name string) int {
	return k.fail(1, "Error from server (NotFound): pods %q not found", name)
}

func (k kubectlCLI) save(rec *podRecord) error {
	if err := os.MkdirAll(k.podDir(), 0o755); err != nil {
		return err
	}
	return writeJSON(k.podPath(rec.Pod.Name), rec)
}

func (k kubectlCLI) listRecords() ([]*podRecord, error) {
	entries, err := os.ReadDir(k.podDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var recs []*podRecord
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		rec, err := k.load(name)
		if err != nil {
			continue
		}
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Pod.Name < recs[j].Pod.Name })
	return recs, nil
}

// locked runs fn under the host lock after reconciling every pod of the
// namespace (the emulated kubelet acts whenever the API is used).
func (k kubectlCLI) locked(fn func() int) int {
	unlock, err := k.host.Lock()
	if err != nil {
		return k.fail(1, "kubectl (emulated): %v", err)
	}
	defer unlock()
	recs, err := k.listRecords()
	if err != nil {
		return k.fail(1, "kubectl (emulated): %v", err)
	}
	for _, rec := range recs {
		if err := k.reconcile(rec); err != nil {
			return k.fail(1, "kubectl (emulated): reconciling pod %s: %v", rec.Pod.Name, err)
		}
	}
	return fn()
}

func initProcessName(c corev1.Container) string { return "init:" + c.Name }
func mainProcessName(c corev1.Container) string { return "container:" + c.Name }

// reconcile advances a pod the way a kubelet does and recomputes its status.
func (k kubectlCLI) reconcile(rec *podRecord) error {
	pod := &rec.Pod
	if rec.Sandbox == "" || rec.Failure != "" {
		return k.save(rec)
	}
	c, err := k.host.Lookup(rec.Sandbox)
	if err != nil {
		return fmt.Errorf("sandbox %s: %w", rec.Sandbox, err)
	}
	spec := pod.Spec
	for rec.NextInit < len(spec.InitContainers) {
		ic := spec.InitContainers[rec.NextInit]
		name := initProcessName(ic)
		if _, started := c.Processes[name]; !started {
			if err := k.startContainer(c, ic, name); err != nil {
				rec.Failure = err.Error()
			}
			break
		}
		if k.host.ProcessRunning(c, name) {
			break
		}
		if k.exitCode(c, name) != 0 {
			break // restartPolicy Never: a failed init container fails the pod
		}
		rec.NextInit++
	}
	if rec.Failure == "" && rec.NextInit == len(spec.InitContainers) && !rec.MainStarted {
		for _, mc := range spec.Containers {
			if err := k.startContainer(c, mc, mainProcessName(mc)); err != nil {
				rec.Failure = err.Error()
				break
			}
		}
		rec.MainStarted = true
	}
	k.computeStatus(rec, c)
	return k.save(rec)
}

// exitCode is a stopped process's status; a process that died without the
// shim recording one was SIGKILLed.
func (k kubectlCLI) exitCode(c *Container, name string) int {
	if code, ok := k.host.ProcessExit(c, name); ok {
		return code
	}
	return 137
}

func (k kubectlCLI) containerEnv(ctr corev1.Container) ([]string, error) {
	img, ok := k.host.LookupImage(ctr.Image)
	if !ok {
		return nil, fmt.Errorf("image %s not present", ctr.Image)
	}
	env := append([]string(nil), img.Env...)
	for _, e := range ctr.Env {
		if e.ValueFrom != nil {
			return nil, fmt.Errorf("container %s: env %s: valueFrom is not supported by the emulated kubelet", ctr.Name, e.Name)
		}
		env = mergeEnv(env, []string{e.Name + "=" + e.Value})
	}
	return env, nil
}

func (k kubectlCLI) startContainer(c *Container, ctr corev1.Container, name string) error {
	argv := append(append([]string(nil), ctr.Command...), ctr.Args...)
	if len(ctr.Command) == 0 {
		return fmt.Errorf("container %s: no command (emulated images have no entrypoint)", ctr.Name)
	}
	env, err := k.containerEnv(ctr)
	if err != nil {
		return err
	}
	_, err = k.host.Start(c, name, ExecOptions{Env: env, WorkingDir: ctr.WorkingDir}, argv)
	return err
}

func (k kubectlCLI) computeStatus(rec *podRecord, c *Container) {
	pod := &rec.Pod
	now := metav1.Now()
	state := func(name string, started bool) (corev1.ContainerState, bool) {
		switch {
		case !started:
			return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}, false
		case k.host.ProcessRunning(c, name):
			return corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: pod.CreationTimestamp}}, true
		default:
			code := k.exitCode(c, name)
			reason := "Completed"
			if code != 0 {
				reason = "Error"
			}
			return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: int32(code), Reason: reason, FinishedAt: now}}, false
		}
	}
	pod.Status.InitContainerStatuses = nil
	initFailed := false
	for _, ic := range pod.Spec.InitContainers {
		_, started := c.Processes[initProcessName(ic)]
		st, _ := state(initProcessName(ic), started)
		if st.Terminated != nil && st.Terminated.ExitCode != 0 {
			initFailed = true
		}
		pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses, corev1.ContainerStatus{
			Name: ic.Name, Image: ic.Image, State: st, Ready: st.Terminated != nil && st.Terminated.ExitCode == 0,
		})
	}
	pod.Status.ContainerStatuses = nil
	anyRunning, allRunning, allSucceeded := false, true, true
	for _, mc := range pod.Spec.Containers {
		st, running := state(mainProcessName(mc), rec.MainStarted)
		anyRunning = anyRunning || running
		allRunning = allRunning && running
		allSucceeded = allSucceeded && st.Terminated != nil && st.Terminated.ExitCode == 0
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
			Name: mc.Name, Image: mc.Image, State: st, Ready: running, Started: &running,
		})
	}
	switch {
	case rec.Failure != "" || initFailed:
		pod.Status.Phase = corev1.PodFailed
		pod.Status.Message = rec.Failure
	case !rec.MainStarted:
		pod.Status.Phase = corev1.PodPending
	case anyRunning:
		pod.Status.Phase = corev1.PodRunning
	case allSucceeded:
		pod.Status.Phase = corev1.PodSucceeded
	default:
		pod.Status.Phase = corev1.PodFailed
	}
	ready := corev1.ConditionFalse
	if rec.MainStarted && allRunning {
		ready = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}
	if pod.Status.StartTime == nil {
		pod.Status.StartTime = &pod.CreationTimestamp
	}
}

// --- apply ---

func (k kubectlCLI) apply(args []string) int {
	var file string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-f" || a == "--filename":
			if i+1 >= len(args) {
				return k.fail(1, "error: flag needs an argument: %s", a)
			}
			i++
			file = args[i]
		case strings.HasPrefix(a, "--filename=") || strings.HasPrefix(a, "-f="):
			_, file, _ = strings.Cut(a, "=")
		default:
			return k.fail(1, "kubectl (emulated): apply: unsupported argument %q", a)
		}
	}
	r := k.stdin
	if file == "" {
		return k.fail(1, "error: must specify one of -f and -k")
	}
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return k.fail(1, "error: the path %q does not exist", file)
		}
		defer f.Close() //nolint:errcheck // read-only
		r = f
	}
	var pod corev1.Pod
	if err := yaml.NewYAMLOrJSONDecoder(r, 4096).Decode(&pod); err != nil {
		return k.fail(1, "error: error parsing %s: %v", file, err)
	}
	if pod.Kind != "Pod" || pod.APIVersion != "v1" {
		return k.fail(1, "kubectl (emulated): only v1 Pods are supported, got %s %s", pod.APIVersion, pod.Kind)
	}
	if pod.Namespace != "" && pod.Namespace != k.namespace {
		return k.fail(1, "error: the namespace from the provided object %q does not match the namespace %q. You must pass '--namespace=%s' to perform this operation.", pod.Namespace, k.namespace, pod.Namespace)
	}
	pod.Namespace = k.namespace
	if pod.Name == "" {
		return k.fail(1, "error: error when retrieving current configuration: resource name may not be empty")
	}
	return k.locked(func() int {
		if existing, err := k.load(pod.Name); err == nil {
			if equalSpec(existing.Pod.Spec, pod.Spec) {
				_, _ = fmt.Fprintf(k.stdout, "pod/%s unchanged\n", pod.Name)
				return 0
			}
			return k.fail(1, "The Pod %q is invalid: spec: Forbidden: pod updates may not change fields other than `spec.containers[*].image`, `spec.initContainers[*].image`, `spec.activeDeadlineSeconds`, `spec.tolerations` (only additions to existing tolerations) or `spec.terminationGracePeriodSeconds` (allow it to be set to 1 if it was previously negative)", pod.Name)
		}
		rec, err := k.create(pod)
		if err != nil {
			return k.fail(1, "Error from server (Invalid): %v", err)
		}
		if err := k.reconcile(rec); err != nil {
			return k.fail(1, "kubectl (emulated): %v", err)
		}
		_, _ = fmt.Fprintf(k.stdout, "pod/%s created\n", pod.Name)
		return 0
	})
}

func equalSpec(a, b corev1.PodSpec) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

func (k kubectlCLI) create(pod corev1.Pod) (*podRecord, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	pod.UID = types.UID(id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:32])
	pod.CreationTimestamp = metav1.Now()
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending}
	rec := &podRecord{Pod: pod}

	all := append(append([]corev1.Container(nil), pod.Spec.InitContainers...), pod.Spec.Containers...)
	if len(pod.Spec.Containers) == 0 {
		return nil, errors.New("spec.containers: Required value")
	}
	volumes := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		if v.EmptyDir == nil && v.Secret == nil {
			return nil, fmt.Errorf("volume %s: only emptyDir and secret volumes are supported by the emulated kubelet", v.Name)
		}
		volumes[v.Name] = true
	}
	roots := append([]string(nil), PodPrivateRoots...)
	type link struct{ path, volume string }
	var links []link
	for _, ctr := range all {
		for _, m := range ctr.VolumeMounts {
			if !volumes[m.Name] {
				return nil, fmt.Errorf("spec.containers[%s].volumeMounts: Not found: %q", ctr.Name, m.Name)
			}
			if !slices.Contains(roots, m.MountPath) {
				roots = append(roots, m.MountPath)
			}
			links = append(links, link{m.MountPath, m.Name})
		}
	}
	for _, ctr := range all {
		if _, ok := k.host.LookupImage(ctr.Image); !ok {
			// Never pullable: the pod stays Pending.
			rec.Pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name:  ctr.Name,
				Image: ctr.Image,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "image " + ctr.Image + " not present on the emulated host"}},
			}}
			return rec, k.save(rec)
		}
	}
	c, err := k.host.NewSandbox(CreateOptions{
		Name:         "k8s_" + k.namespace + "_" + pod.Name + "_" + id[:8],
		Image:        pod.Spec.Containers[0].Image,
		Labels:       pod.Labels,
		WorkingDir:   "/",
		PrivateRoots: roots,
	})
	if err != nil {
		return nil, err
	}
	// Each volume is one directory; every mount path of it is a link to
	// that directory, so containers mounting it at different paths share it.
	for _, l := range links {
		vol := filepath.Join(k.host.containerDir(c.ID), "volumes", l.volume)
		if err := os.MkdirAll(vol, 0o755); err != nil {
			return nil, err
		}
		at := k.host.HostPath(c, l.path)
		if err := os.RemoveAll(at); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			return nil, err
		}
		if err := os.Symlink(vol, at); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	rec.Sandbox = c.ID
	return rec, k.save(rec)
}

// --- get ---

// resourceArgs splits "pod NAME..." / "pods" / "pod/NAME..." arguments.
func resourceArgs(pos []string) (names []string, err error) {
	if len(pos) == 0 {
		return nil, errors.New("you must specify the type of resource to get")
	}
	if !strings.Contains(pos[0], "/") {
		if !isPodKind(pos[0]) {
			return nil, fmt.Errorf("the emulated API server serves pods only, not %q", pos[0])
		}
		return pos[1:], nil
	}
	for _, p := range pos {
		kind, name, ok := strings.Cut(p, "/")
		if !ok || !isPodKind(kind) || name == "" {
			return nil, fmt.Errorf("the emulated API server serves pods only, not %q", p)
		}
		names = append(names, name)
	}
	return names, nil
}

func isPodKind(kind string) bool {
	return kind == "pod" || kind == "pods" || kind == "po"
}

type selectorFlags struct {
	label, field, output string
	ignoreNotFound       bool
}

func (k kubectlCLI) parseGetFlags(args []string, extra map[string]*string, extraBools map[string]*bool) ([]string, selectorFlags, error) {
	var f selectorFlags
	values := map[string]*string{
		"-l": &f.label, "--selector": &f.label,
		"--field-selector": &f.field,
		"-o":               &f.output, "--output": &f.output,
	}
	for n, p := range extra {
		values[n] = p
	}
	bools := map[string]*bool{"--ignore-not-found": &f.ignoreNotFound}
	for n, p := range extraBools {
		bools[n] = p
	}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
			continue
		}
		name, value, hasValue := strings.Cut(a, "=")
		if p, ok := bools[name]; ok {
			*p = !hasValue || value == "true"
			continue
		}
		p, ok := values[name]
		if !ok {
			return nil, f, fmt.Errorf("unknown flag: %s", name)
		}
		if !hasValue {
			if i+1 >= len(args) {
				return nil, f, fmt.Errorf("flag needs an argument: %s", name)
			}
			i++
			value = args[i]
		}
		*p = value
	}
	return pos, f, nil
}

func podFields(p *corev1.Pod) fields.Set {
	return fields.Set{
		"metadata.name":      p.Name,
		"metadata.namespace": p.Namespace,
		"status.phase":       string(p.Status.Phase),
		"spec.nodeName":      p.Spec.NodeName,
		"spec.restartPolicy": string(p.Spec.RestartPolicy),
	}
}

func (k kubectlCLI) selectPods(f selectorFlags) ([]*podRecord, error) {
	ls, err := labels.Parse(f.label)
	if err != nil {
		return nil, err
	}
	fs, err := fields.ParseSelector(f.field)
	if err != nil {
		return nil, err
	}
	for _, r := range fs.Requirements() {
		if _, ok := podFields(&corev1.Pod{})[r.Field]; !ok {
			return nil, fmt.Errorf("field label not supported: %s", r.Field)
		}
	}
	recs, err := k.listRecords()
	if err != nil {
		return nil, err
	}
	var out []*podRecord
	for _, rec := range recs {
		if ls.Matches(labels.Set(rec.Pod.Labels)) && fs.Matches(podFields(&rec.Pod)) {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (k kubectlCLI) get(args []string) int {
	pos, f, err := k.parseGetFlags(args, nil, nil)
	if err != nil {
		return k.fail(1, "error: %v", err)
	}
	names, err := resourceArgs(pos)
	if err != nil {
		return k.fail(1, "error: %v", err)
	}
	return k.locked(func() int {
		var pods []corev1.Pod
		single := len(names) == 1
		if len(names) > 0 {
			if f.label != "" || f.field != "" {
				return k.fail(1, "error: when paths, URLs, or stdin is provided as input, you may not specify resources as arguments as well")
			}
			for _, n := range names {
				rec, err := k.load(n)
				if err != nil {
					if f.ignoreNotFound {
						continue
					}
					return k.notFound(n)
				}
				pods = append(pods, rec.Pod)
			}
		} else {
			recs, err := k.selectPods(f)
			if err != nil {
				return k.fail(1, "Error from server (BadRequest): %v", err)
			}
			for _, rec := range recs {
				pods = append(pods, rec.Pod)
			}
		}
		for i := range pods {
			pods[i].APIVersion, pods[i].Kind = "v1", "Pod"
		}
		// An empty list prints "items": [], as kubectl does.
		var obj any = &corev1.List{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "List"}, Items: []k8sruntime.RawExtension{}}
		if single && len(pods) == 1 {
			obj = &pods[0]
		} else {
			list := obj.(*corev1.List)
			for i := range pods {
				raw, err := json.Marshal(&pods[i])
				if err != nil {
					return k.fail(1, "error: %v", err)
				}
				list.Items = append(list.Items, k8sruntime.RawExtension{Raw: raw})
			}
		}
		return k.print(f.output, obj, pods)
	})
}

func (k kubectlCLI) print(output string, obj any, pods []corev1.Pod) int {
	switch {
	case output == "json":
		b, err := json.MarshalIndent(obj, "", "    ")
		if err != nil {
			return k.fail(1, "error: %v", err)
		}
		_, _ = fmt.Fprintln(k.stdout, string(b))
	case output == "name":
		for _, p := range pods {
			_, _ = fmt.Fprintf(k.stdout, "pod/%s\n", p.Name)
		}
	case strings.HasPrefix(output, "jsonpath="):
		raw, err := json.Marshal(obj)
		if err != nil {
			return k.fail(1, "error: %v", err)
		}
		var data any
		if err := json.Unmarshal(raw, &data); err != nil {
			return k.fail(1, "error: %v", err)
		}
		jp := jsonpath.New("out").AllowMissingKeys(true)
		if err := jp.Parse(strings.TrimPrefix(output, "jsonpath=")); err != nil {
			return k.fail(1, "error: error parsing jsonpath %s, %v", strings.TrimPrefix(output, "jsonpath="), err)
		}
		var buf bytes.Buffer
		if err := jp.Execute(&buf, data); err != nil {
			return k.fail(1, "error: error executing jsonpath %q: %v. Printing more information for debugging the template:\n\ttemplate was:\n\t\t%s", strings.TrimPrefix(output, "jsonpath="), err, strings.TrimPrefix(output, "jsonpath="))
		}
		_, _ = k.stdout.Write(buf.Bytes())
	case output == "":
		if len(pods) == 0 {
			_, _ = fmt.Fprintf(k.stderr, "No resources found in %s namespace.\n", k.namespace)
			return 0
		}
		_, _ = fmt.Fprintln(k.stdout, "NAME\tSTATUS")
		for _, p := range pods {
			_, _ = fmt.Fprintf(k.stdout, "%s\t%s\n", p.Name, p.Status.Phase)
		}
	default:
		return k.fail(1, "kubectl (emulated): unsupported output format %q", output)
	}
	return 0
}

// --- delete ---

func (k kubectlCLI) delete(args []string) int {
	grace := "30"
	var wait string
	var now bool
	pos, f, err := k.parseGetFlags(args,
		map[string]*string{"--grace-period": &grace, "--wait": &wait},
		map[string]*bool{"--now": &now})
	if err != nil {
		return k.fail(1, "error: %v", err)
	}
	names, err := resourceArgs(pos)
	if err != nil {
		return k.fail(1, "error: %v", err)
	}
	seconds, err := strconv.Atoi(grace)
	if err != nil {
		return k.fail(1, "error: invalid argument %q for \"--grace-period\" flag", grace)
	}
	if now || seconds < 0 {
		seconds = 1
	}
	return k.locked(func() int {
		var recs []*podRecord
		if len(names) > 0 {
			for _, n := range names {
				rec, err := k.load(n)
				if err != nil {
					if f.ignoreNotFound {
						continue
					}
					return k.notFound(n)
				}
				recs = append(recs, rec)
			}
		} else {
			if f.label == "" && f.field == "" {
				return k.fail(1, "error: resource(s) were provided, but no name was specified")
			}
			recs, err = k.selectPods(f)
			if err != nil {
				return k.fail(1, "Error from server (BadRequest): %v", err)
			}
			if len(recs) == 0 && !f.ignoreNotFound {
				_, _ = fmt.Fprintf(k.stderr, "No resources found\n")
			}
		}
		for _, rec := range recs {
			if rec.Sandbox != "" {
				if c, err := k.host.Lookup(rec.Sandbox); err == nil {
					if err := k.host.Stop(c, time.Duration(seconds)*time.Second); err != nil {
						return k.fail(1, "kubectl (emulated): %v", err)
					}
					if err := k.host.Remove(c); err != nil {
						return k.fail(1, "kubectl (emulated): %v", err)
					}
				}
			}
			if err := os.Remove(k.podPath(rec.Pod.Name)); err != nil {
				return k.fail(1, "kubectl (emulated): %v", err)
			}
			_, _ = fmt.Fprintf(k.stdout, "pod %q deleted\n", rec.Pod.Name)
		}
		return 0
	})
}

// --- wait ---

func (k kubectlCLI) wait(args []string) int {
	var forCond, timeout string
	timeout = "30s"
	pos, _, err := k.parseGetFlags(args, map[string]*string{"--for": &forCond, "--timeout": &timeout}, nil)
	if err != nil {
		return k.fail(1, "error: %v", err)
	}
	names, err := resourceArgs(pos)
	if err != nil || len(names) != 1 {
		return k.fail(1, "error: wait needs exactly one pod/NAME (%v)", err)
	}
	d, err := time.ParseDuration(timeout)
	if err != nil {
		return k.fail(1, "error: invalid timeout %q", timeout)
	}
	name := names[0]
	deadline := time.Now().Add(d)
	for {
		var done bool
		code := k.locked(func() int {
			rec, err := k.load(name)
			switch {
			case forCond == "delete":
				done = errors.Is(err, errPodNotFound)
			case err != nil:
				return k.notFound(name)
			case forCond == "condition=Ready":
				for _, c := range rec.Pod.Status.Conditions {
					done = done || (c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue)
				}
			default:
				return k.fail(1, "kubectl (emulated): unsupported --for=%s", forCond)
			}
			return 0
		})
		if code != 0 {
			return code
		}
		if done {
			if forCond == "delete" {
				return 0
			}
			_, _ = fmt.Fprintf(k.stdout, "pod/%s condition met\n", name)
			return 0
		}
		if time.Now().After(deadline) {
			return k.fail(1, "error: timed out waiting for the condition on pods/%s", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// --- exec / cp ---

// target resolves a running container of a pod: the named one, or the
// first of spec.containers (kubectl's default).
func (k kubectlCLI) target(podName, container string) (*Container, corev1.Container, int) {
	var c *Container
	var ctr corev1.Container
	var proc string // the process backing the chosen container
	code := k.locked(func() int {
		rec, err := k.load(podName)
		if err != nil {
			return k.notFound(podName)
		}
		found := false
		for _, ic := range rec.Pod.Spec.InitContainers {
			if ic.Name == container {
				ctr, proc, found = ic, initProcessName(ic), true
			}
		}
		for i, mc := range rec.Pod.Spec.Containers {
			if mc.Name == container || (container == "" && i == 0) {
				ctr, proc, found = mc, mainProcessName(mc), true
			}
		}
		if !found {
			return k.fail(1, "Error from server (BadRequest): container %s is not valid for pod %s", container, podName)
		}
		if rec.Sandbox == "" {
			return k.fail(1, "error: unable to upgrade connection: container not found (%q)", ctr.Name)
		}
		c, err = k.host.Lookup(rec.Sandbox)
		if err != nil || !k.host.ProcessRunning(c, proc) {
			return k.fail(1, "error: unable to upgrade connection: container not found (%q)", ctr.Name)
		}
		return 0
	})
	return c, ctr, code
}

func (k kubectlCLI) exec(args []string) int {
	var interactive, tty bool
	var container string
	fs := newFlagSet()
	fs.boolFlag(&interactive, "-i", "--stdin")
	fs.boolFlag(&tty, "-t", "--tty")
	fs.str(&container, "-c", "--container")
	var pos []string
	dash := slices.Index(args, "--")
	if dash < 0 {
		return k.fail(1, "error: exec needs `-- COMMAND` (the emulated kubectl does not accept the deprecated form)")
	}
	pos, err := fs.parse(args[:dash], false)
	if err != nil || len(pos) != 1 {
		return k.fail(1, "error: exec: %v (arguments %q)", err, pos)
	}
	argv := args[dash+1:]
	if len(argv) == 0 {
		return k.fail(1, "error: you must specify at least one command for the container")
	}
	c, ctr, code := k.target(strings.TrimPrefix(pos[0], "pod/"), container)
	if code != 0 {
		return code
	}
	env, err := k.containerEnv(ctr)
	if err != nil {
		return k.fail(1, "error: %v", err)
	}
	cmd, err := k.host.Command(c, ExecOptions{Env: env, WorkingDir: ctr.WorkingDir}, argv)
	if err != nil {
		_, _ = fmt.Fprintf(k.stderr, "error: Internal error occurred: error executing command in container: %v\n", err)
		return k.fail(126, "command terminated with exit code 126")
	}
	if interactive {
		cmd.Stdin = k.stdin
	}
	cmd.Stdout, cmd.Stderr = k.stdout, k.stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return k.fail(1, "error: %v", err)
	}
	if status := exitStatus(err); status != 0 {
		return k.fail(status, "command terminated with exit code %d", status)
	}
	return 0
}

func (k kubectlCLI) cp(args []string) int {
	var container string
	fs := newFlagSet()
	fs.str(&container, "-c", "--container")
	pos, err := fs.parse(args, false)
	if err != nil || len(pos) != 2 {
		return k.fail(1, "error: cp: %v (arguments %q)", err, pos)
	}
	src, dst := pos[0], pos[1]
	podName, podPath, ok := strings.Cut(dst, ":")
	if !ok || strings.Contains(src, ":") {
		return k.fail(1, "kubectl (emulated): cp supports local -> pod copies only")
	}
	c, _, code := k.target(podName, container)
	if code != 0 {
		return code
	}
	to := k.host.HostPath(c, podPath)
	contents := strings.HasSuffix(src, "/.")
	st, err := os.Stat(src)
	if err != nil {
		return k.fail(1, "error: %s doesn't exist in local filesystem", src)
	}
	switch {
	case st.IsDir() && contents:
		err = copyTree(strings.TrimSuffix(src, "/."), to)
	case st.IsDir():
		if dt, derr := os.Stat(to); derr == nil && dt.IsDir() {
			to = filepath.Join(to, filepath.Base(src))
		}
		err = copyTree(src, to)
	default:
		if dt, derr := os.Stat(to); derr == nil && dt.IsDir() {
			to = filepath.Join(to, filepath.Base(src))
		}
		err = copyFile(src, to, st.Mode())
	}
	if err != nil {
		return k.fail(1, "error: %v", err)
	}
	return 0
}

func copyTree(from, to string) error {
	return filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode())
		default:
			return nil // sockets, devices: tar skips them too
		}
	})
}

func copyFile(from, to string, mode os.FileMode) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.WriteFile(to, data, mode.Perm())
}
