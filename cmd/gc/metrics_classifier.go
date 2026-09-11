package main

import (
	"encoding/csv"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type productMetricsPolicyContext struct {
	ManagedAutomation bool
	ProviderHook      bool
}

type productMetricsInvocationArgs struct {
	Raw     []string
	Command []string
}

type productMetricsClassification struct {
	ID        productMetricsCommandID
	Notice    productMetricsNoticePolicy
	Recording productMetricsRecordingPolicy
	Owner     productMetricsOwner
	Exclusion productMetricsExclusionReason
	Resolver  productMetricsResolverKey
}

type productMetricsPolicyDecision struct {
	Notice    productMetricsNoticePolicy
	Recording productMetricsRecordingPolicy
	Exclusion productMetricsExclusionReason
}

type productMetricsStaticModeRegistration struct {
	Mode    productMetricsMode
	Resolve func() productMetricsPolicyDecision
}

var productMetricsStaticModeRegistry = []productMetricsStaticModeRegistration{
	{Mode: productMetricsModeStandard, Resolve: eligibleRecordablePolicy},
	{Mode: productMetricsModeCompletion, Resolve: ineligibleRecordablePolicy},
	{Mode: productMetricsModeVersion, Resolve: ineligibleRecordablePolicy},
	{Mode: productMetricsModeBdPassthrough, Resolve: ineligibleRecordablePolicy},
	{Mode: productMetricsModeEventsStream, Resolve: ineligibleRecordablePolicy},
	{Mode: productMetricsModePerfWrapper, Resolve: ineligibleRecordablePolicy},
	{Mode: productMetricsModeWorkflowCompat, Resolve: ineligibleRecordablePolicy},
	{Mode: productMetricsModeSupervisorService, Resolve: ineligibleRecordablePolicy},
	{Mode: productMetricsModePackCommand, Resolve: ineligibleRecordablePolicy},
	{Mode: productMetricsModeHiddenPrivate, Resolve: func() productMetricsPolicyDecision { return excludedPolicy(productMetricsExclusionHiddenPrivate) }},
	{Mode: productMetricsModeMetricsControl, Resolve: func() productMetricsPolicyDecision { return excludedPolicy(productMetricsExclusionMetricsControl) }},
	{Mode: productMetricsModeHookProtocol, Resolve: func() productMetricsPolicyDecision { return excludedPolicy(productMetricsExclusionHookProtocol) }},
	{Mode: productMetricsModeEventEmit, Resolve: func() productMetricsPolicyDecision { return excludedPolicy(productMetricsExclusionEventEmit) }},
	{Mode: productMetricsModeCredentialHelper, Resolve: func() productMetricsPolicyDecision { return excludedPolicy(productMetricsExclusionCredentialHelper) }},
	{Mode: productMetricsModePrivateCompletion, Resolve: func() productMetricsPolicyDecision { return excludedPolicy(productMetricsExclusionPrivateCompletion) }},
}

type productMetricsConditionalRegistration struct {
	Mode  productMetricsConditionalMode
	Apply func(*cobra.Command, productMetricsInvocationArgs, productMetricsPolicyContext) (productMetricsPolicyDecision, bool)
}

var productMetricsConditionalRegistry = []productMetricsConditionalRegistration{
	{Mode: productMetricsConditionalGenericMachineOutput, Apply: applyGenericMachineOutputPolicy},
	{Mode: productMetricsConditionalManagedContext, Apply: func(_ *cobra.Command, _ productMetricsInvocationArgs, context productMetricsPolicyContext) (productMetricsPolicyDecision, bool) {
		return excludedPolicy(productMetricsExclusionManagedContext), context.ManagedAutomation
	}},
	{Mode: productMetricsConditionalProviderHook, Apply: func(_ *cobra.Command, _ productMetricsInvocationArgs, context productMetricsPolicyContext) (productMetricsPolicyDecision, bool) {
		return excludedPolicy(productMetricsExclusionProviderHook), context.ProviderHook
	}},
	{Mode: productMetricsConditionalBeadsMachineOutput, Apply: applyBeadsMachineOutputPolicy},
	{Mode: productMetricsConditionalPrimeHook, Apply: func(command *cobra.Command, args productMetricsInvocationArgs, _ productMetricsPolicyContext) (productMetricsPolicyDecision, bool) {
		matched := literalBoolFlagEnabled(command, args.Command, "hook") || literalStringFlagNonempty(command, args.Command, "hook-format")
		return excludedPolicy(productMetricsExclusionPrimeHook), matched
	}},
	{Mode: productMetricsConditionalHandoffAutomation, Apply: func(command *cobra.Command, args productMetricsInvocationArgs, _ productMetricsPolicyContext) (productMetricsPolicyDecision, bool) {
		matched := literalBoolFlagEnabled(command, args.Command, "auto") || literalStringFlagNonempty(command, args.Command, "hook-format")
		return excludedPolicy(productMetricsExclusionHandoffAutomation), matched
	}},
	{Mode: productMetricsConditionalMailHookFormat, Apply: func(command *cobra.Command, args productMetricsInvocationArgs, _ productMetricsPolicyContext) (productMetricsPolicyDecision, bool) {
		matched := literalBoolFlagEnabled(command, args.Command, "inject") || literalStringFlagNonempty(command, args.Command, "hook-format")
		return excludedPolicy(productMetricsExclusionMailHookFormat), matched
	}},
}

type productMetricsDeferredResolver func(productMetricsClassification, bool, bool) productMetricsClassification

type productMetricsResolverRegistration struct {
	Key     productMetricsResolverKey
	Resolve productMetricsDeferredResolver
}

var productMetricsResolverRegistry = []productMetricsResolverRegistration{
	{Key: productMetricsResolverRootDispatch, Resolve: resolveDeferredCommand},
	{Key: productMetricsResolverGroupDispatch, Resolve: resolveDeferredCommand},
	{Key: productMetricsResolverPackDispatch, Resolve: resolveDeferredCommand},
}

func eligibleRecordablePolicy() productMetricsPolicyDecision {
	return productMetricsPolicyDecision{Notice: productMetricsNoticeEligible, Recording: productMetricsRecordingRecordable}
}

func ineligibleRecordablePolicy() productMetricsPolicyDecision {
	return productMetricsPolicyDecision{Notice: productMetricsNoticeIneligible, Recording: productMetricsRecordingRecordable}
}

func excludedPolicy(reason productMetricsExclusionReason) productMetricsPolicyDecision {
	return productMetricsPolicyDecision{Notice: productMetricsNoticeIneligible, Recording: productMetricsRecordingExcluded, Exclusion: reason}
}

func classifyProductMetricsCommand(root *cobra.Command, args []string, context productMetricsPolicyContext) productMetricsClassification {
	if root == nil || root.Annotations[productMetricsCensusValidAnnotation] != "true" {
		return failClosedProductMetricsClassification()
	}
	selection := resolveProductMetricsSelection(root, args)
	if selection.privateCompletion {
		return classificationFromSynthetic(productMetricsExclusionPrivateCompletion)
	}
	if selection.command == nil {
		return classificationFromSynthetic("")
	}
	invocationArgs := productMetricsInvocationArgs{Raw: args, Command: selection.commandArgs}
	if selection.command.Annotations[productMetricsClassAnnotation] == packCommandClassificationValue && !productMetricsBuiltInPath(selection.command.CommandPath()) {
		classification := applyProductMetricsPolicies(selection.command, invocationArgs, context, classificationFromSyntheticPack())
		if classification.Recording == productMetricsRecordingExcluded {
			classification.ID, classification.Owner, classification.Resolver = 0, productMetricsOwnerExcluded, ""
		}
		return classification
	}
	classification, ok := classificationFromCommandAnnotations(selection.command)
	if !ok {
		return failClosedProductMetricsClassification()
	}
	if classification.Recording == productMetricsRecordingExcluded {
		classification.ID = 0
		classification.Owner = productMetricsOwnerExcluded
		classification.Resolver = ""
		return classification
	}
	classification = applyProductMetricsPolicies(selection.command, invocationArgs, context, classification)
	if classification.Recording == productMetricsRecordingExcluded {
		classification.ID = 0
		classification.Owner = productMetricsOwnerExcluded
		classification.Resolver = ""
		return classification
	}
	if selection.helpRequested {
		classification.ID = productMetricsCommandHelp
		return classification
	}
	if selection.unresolved {
		classification.ID = productMetricsCommandUnknown
		return classification
	}
	if classification.Owner == productMetricsOwnerDeferred {
		if resolver, ok := lookupProductMetricsResolver(classification.Resolver); ok {
			classification = resolver(classification, selection.bare, selection.unresolved)
		} else {
			return failClosedProductMetricsClassification()
		}
	}
	return classification
}

func classifyProductMetricsPackOutcome(outcome packCommandOutcome, context productMetricsPolicyContext) productMetricsClassification {
	classification := classificationFromSynthetic("")
	if outcome.classification == packCommandClassification {
		classification = classificationFromSyntheticPack()
	}
	if context.ManagedAutomation {
		classification.ID, classification.Notice, classification.Recording, classification.Owner, classification.Exclusion, classification.Resolver = 0, productMetricsNoticeIneligible, productMetricsRecordingExcluded, productMetricsOwnerExcluded, productMetricsExclusionManagedContext, ""
	} else if context.ProviderHook {
		classification.ID, classification.Notice, classification.Recording, classification.Owner, classification.Exclusion, classification.Resolver = 0, productMetricsNoticeIneligible, productMetricsRecordingExcluded, productMetricsOwnerExcluded, productMetricsExclusionProviderHook, ""
	}
	return classification
}

type productMetricsSelection struct {
	command           *cobra.Command
	commandArgs       []string
	bare              bool
	unresolved        bool
	helpRequested     bool
	privateCompletion bool
}

func resolveProductMetricsSelection(root *cobra.Command, args []string) productMetricsSelection {
	if root == nil {
		return productMetricsSelection{}
	}
	if request, ok := parseJSONSchemaRequest(args); ok {
		command, commandArgs, findErr := root.Find(request.commandArgs)
		unresolved := findErr != nil || command == nil || (command == root && len(request.commandArgs) > 0)
		return newProductMetricsSelection(command, commandArgs, unresolved, false)
	}

	jsonRequest, jsonDisposition := resolveJSONContractDisposition(root, args)
	if jsonDisposition == jsonContractCommandNotFound || jsonDisposition == jsonContractUnsupported {
		filteredArgs, _ := filterJSONFlag(args)
		command, _, _ := root.Find(filteredArgs)
		if jsonRequest.cmd != nil {
			command = jsonRequest.cmd
		}
		_, commandArgs, _ := root.Find(args)
		return newProductMetricsSelection(command, commandArgs, jsonDisposition == jsonContractCommandNotFound, false)
	}

	if privateProductMetricsCompletionRequested(root, args) {
		return productMetricsSelection{privateCompletion: true}
	}
	command, commandArgs, findErr := root.Find(args)
	scan := scanProductMetricsCommandArgs(command, commandArgs)
	unresolved := command == nil
	if command != nil && !scan.parseFailed {
		unresolved = findErr != nil || (command.HasSubCommands() && scan.positionalCount > 0)
	}
	return newProductMetricsSelection(command, commandArgs, unresolved, scan.helpRequested && !scan.parseFailed)
}

func privateProductMetricsCompletionRequested(root *cobra.Command, args []string) bool {
	for index := 0; index < len(args); index++ {
		token := args[index]
		if token == "--" {
			return false
		}
		if strings.HasPrefix(token, "--") {
			name, _, hasValue := splitLongFlag(token)
			flag := lookupCommandFlag(root, name)
			if !hasValue && (flag == nil || flag.NoOptDefVal == "") && index+1 < len(args) {
				index++
			}
			continue
		}
		if strings.HasPrefix(token, "-") && token != "-" {
			if len(token) == 2 {
				flag := lookupCommandShorthand(root, token[1:])
				if (flag == nil || flag.NoOptDefVal == "") && index+1 < len(args) {
					index++
				}
			}
			continue
		}
		return token == "__complete" || token == "__completeNoDesc"
	}
	return false
}

func newProductMetricsSelection(command *cobra.Command, commandArgs []string, unresolved, helpRequested bool) productMetricsSelection {
	bare := !unresolved && command != nil && (command == command.Root() || command.HasSubCommands())
	return productMetricsSelection{
		command:       command,
		commandArgs:   append([]string(nil), commandArgs...),
		bare:          bare,
		unresolved:    unresolved,
		helpRequested: helpRequested,
	}
}

type productMetricsCommandArgScan struct {
	helpRequested   bool
	parseFailed     bool
	positionalCount int
}

type productMetricsParsedFlag struct {
	flag  *pflag.Flag
	name  string
	value string
}

func scanProductMetricsCommandArgs(command *cobra.Command, args []string) productMetricsCommandArgScan {
	var scan productMetricsCommandArgScan
	if command == nil {
		scan.parseFailed = true
		return scan
	}
	if command.DisableFlagParsing {
		scan.positionalCount = len(args)
		return scan
	}
	terminated := false
	for index := 0; index < len(args); index++ {
		token := args[index]
		if !terminated && token == "--" {
			terminated = true
			continue
		}
		if terminated || !strings.HasPrefix(token, "-") || token == "-" {
			scan.positionalCount++
			continue
		}
		parsedFlags, consumed, ok := parseProductMetricsLiteralFlagToken(command, token, args[index+1:])
		if !ok {
			scan.parseFailed = true
			break
		}
		index += consumed
		for _, parsed := range parsedFlags {
			if !validProductMetricsParsedFlag(parsed) {
				scan.parseFailed = true
				break
			}
			if parsed.name == "help" {
				scan.helpRequested, _ = strconv.ParseBool(parsed.value)
			}
		}
		if scan.parseFailed {
			break
		}
	}
	return scan
}

func parseProductMetricsLiteralFlagToken(command *cobra.Command, token string, following []string) ([]productMetricsParsedFlag, int, bool) {
	if strings.HasPrefix(token, "--") {
		name, value, hasValue := splitLongFlag(token)
		if name == "" {
			return nil, 0, false
		}
		flag := lookupCommandFlag(command, name)
		if flag == nil && name == "help" {
			if !hasValue {
				value = "true"
			}
			return []productMetricsParsedFlag{{name: "help", value: value}}, 0, true
		}
		if flag == nil {
			return nil, 0, false
		}
		consumed := 0
		if !hasValue {
			switch {
			case flag.NoOptDefVal != "":
				value = flag.NoOptDefVal
			case len(following) > 0:
				value, consumed = following[0], 1
			default:
				return nil, 0, false
			}
		}
		return []productMetricsParsedFlag{{flag: flag, name: flag.Name, value: value}}, consumed, true
	}
	if !strings.HasPrefix(token, "-") || len(token) < 2 {
		return nil, 0, false
	}
	shorthands := token[1:]
	parsed := make([]productMetricsParsedFlag, 0, len(shorthands))
	consumed := 0
	for len(shorthands) > 0 {
		shorthand := shorthands[:1]
		shorthands = shorthands[1:]
		flag := lookupCommandShorthand(command, shorthand)
		if flag == nil && shorthand == "h" {
			value := "true"
			if strings.HasPrefix(shorthands, "=") {
				value, shorthands = strings.TrimPrefix(shorthands, "="), ""
			}
			parsed = append(parsed, productMetricsParsedFlag{name: "help", value: value})
			continue
		}
		if flag == nil {
			return nil, 0, false
		}
		value := ""
		switch {
		case strings.HasPrefix(shorthands, "="):
			value, shorthands = strings.TrimPrefix(shorthands, "="), ""
		case flag.NoOptDefVal != "":
			value = flag.NoOptDefVal
		case shorthands != "":
			value, shorthands = shorthands, ""
		case len(following) > 0 && consumed == 0:
			value, consumed = following[0], 1
		default:
			return nil, 0, false
		}
		parsed = append(parsed, productMetricsParsedFlag{flag: flag, name: flag.Name, value: value})
	}
	return parsed, consumed, true
}

func validProductMetricsParsedFlag(parsed productMetricsParsedFlag) bool {
	if parsed.name == "help" && parsed.flag == nil {
		_, err := strconv.ParseBool(parsed.value)
		return err == nil
	}
	return validProductMetricsFlagValue(parsed.flag, parsed.value)
}

func validProductMetricsFlagValue(flag *pflag.Flag, value string) bool {
	if flag == nil || flag.Value == nil {
		return false
	}
	switch flag.Value.Type() {
	case "bool":
		_, err := strconv.ParseBool(value)
		return err == nil
	case "count", "int", "int8", "int16", "int32", "int64":
		_, err := strconv.ParseInt(value, 0, 64)
		return err == nil
	case "uint", "uint8", "uint16", "uint32", "uint64":
		_, err := strconv.ParseUint(value, 0, 64)
		return err == nil
	case "float32", "float64":
		_, err := strconv.ParseFloat(value, 64)
		return err == nil
	case "duration":
		_, err := time.ParseDuration(value)
		return err == nil
	case "stringSlice":
		if value == "" {
			return true
		}
		_, err := csv.NewReader(strings.NewReader(value)).Read()
		return err == nil
	default:
		return true
	}
}

func classificationFromCommandAnnotations(command *cobra.Command) (productMetricsClassification, bool) {
	annotations := command.Annotations
	if annotations == nil {
		return productMetricsClassification{}, false
	}
	var expected productMetricsCommandCensusEntry
	found := false
	for _, entry := range generatedProductMetricsCommandCensus {
		if entry.Path == command.CommandPath() {
			expected, found = entry, true
			break
		}
	}
	if !found || !commandAnnotationsMatchCensus(annotations, expected) {
		return productMetricsClassification{}, false
	}
	mode := productMetricsMode(annotations[productMetricsModeAnnotation])
	decision, ok := lookupProductMetricsStaticMode(mode)
	if !ok || decision.Notice != productMetricsNoticePolicy(annotations[productMetricsNoticeAnnotation]) || decision.Recording != productMetricsRecordingPolicy(annotations[productMetricsRecordingAnnotation]) || decision.Exclusion != productMetricsExclusionReason(annotations[productMetricsExclusionAnnotation]) {
		return productMetricsClassification{}, false
	}
	id := productMetricsCommandID(0)
	if rawID := annotations[productMetricsIDAnnotation]; rawID != "" {
		parsed, err := strconv.ParseUint(rawID, 10, 16)
		if err != nil {
			return productMetricsClassification{}, false
		}
		id = productMetricsCommandID(parsed)
	}
	owner := productMetricsOwner(annotations[productMetricsOwnerAnnotation])
	resolver := productMetricsResolverKey(annotations[productMetricsResolverAnnotation])
	exclusion := decision.Exclusion
	validOwner := false
	switch owner {
	case productMetricsOwnerStructural, productMetricsOwnerImmediate:
		validOwner = resolver == "" && decision.Recording == productMetricsRecordingRecordable
	case productMetricsOwnerDeferred:
		_, registered := lookupProductMetricsResolver(resolver)
		validOwner = resolver != "" && registered && decision.Recording == productMetricsRecordingRecordable
	case productMetricsOwnerExcluded:
		validOwner = resolver == "" && decision.Recording == productMetricsRecordingExcluded
	}
	if !validOwner || (decision.Recording == productMetricsRecordingRecordable && (id == 0 || !isKnownProductMetricsCommandID(id) || exclusion != "")) ||
		(decision.Recording == productMetricsRecordingExcluded && (id != 0 || exclusion == "")) {
		return productMetricsClassification{}, false
	}
	return productMetricsClassification{ID: id, Notice: decision.Notice, Recording: decision.Recording, Owner: owner, Exclusion: exclusion, Resolver: resolver}, true
}

func commandAnnotationsMatchCensus(annotations map[string]string, entry productMetricsCommandCensusEntry) bool {
	wantID := ""
	if entry.ID != 0 {
		wantID = strconv.FormatUint(uint64(entry.ID), 10)
	}
	wantConditional := ""
	if len(entry.ConditionalModes) > 0 {
		parts := make([]string, len(entry.ConditionalModes))
		for index, mode := range entry.ConditionalModes {
			parts[index] = string(mode)
		}
		wantConditional = strings.Join(parts, ",")
	}
	return annotations[productMetricsClassAnnotation] == entry.Classification &&
		annotations[productMetricsModeAnnotation] == string(entry.Mode) &&
		annotations[productMetricsNoticeAnnotation] == string(entry.Notice) &&
		annotations[productMetricsRecordingAnnotation] == string(entry.Recording) &&
		annotations[productMetricsOwnerAnnotation] == string(entry.Owner) &&
		annotations[productMetricsResolverAnnotation] == string(entry.Resolver) &&
		annotations[productMetricsExclusionAnnotation] == string(entry.Exclusion) &&
		annotations[productMetricsConditionalAnnotation] == wantConditional &&
		annotations[productMetricsIDAnnotation] == wantID
}

func applyProductMetricsPolicies(command *cobra.Command, args productMetricsInvocationArgs, context productMetricsPolicyContext, classification productMetricsClassification) productMetricsClassification {
	modes := append([]productMetricsConditionalMode(nil), generatedProductMetricsGlobalConditionalModes...)
	if encoded := command.Annotations[productMetricsConditionalAnnotation]; encoded != "" {
		for _, value := range strings.Split(encoded, ",") {
			modes = append(modes, productMetricsConditionalMode(value))
		}
	}
	for _, mode := range modes {
		registration, ok := lookupProductMetricsConditional(mode)
		if !ok {
			return failClosedProductMetricsClassification()
		}
		decision, matched := registration(command, args, context)
		if !matched {
			continue
		}
		classification.Notice = decision.Notice
		if decision.Recording == productMetricsRecordingExcluded {
			classification.Recording = decision.Recording
			classification.Exclusion = decision.Exclusion
			break
		}
	}
	return classification
}

func resolveDeferredCommand(classification productMetricsClassification, _ bool, _ bool) productMetricsClassification {
	return classification
}

func lookupProductMetricsStaticMode(mode productMetricsMode) (productMetricsPolicyDecision, bool) {
	for _, registration := range productMetricsStaticModeRegistry {
		if registration.Mode == mode && registration.Resolve != nil {
			return registration.Resolve(), true
		}
	}
	return productMetricsPolicyDecision{}, false
}

func lookupProductMetricsConditional(mode productMetricsConditionalMode) (func(*cobra.Command, productMetricsInvocationArgs, productMetricsPolicyContext) (productMetricsPolicyDecision, bool), bool) {
	for _, registration := range productMetricsConditionalRegistry {
		if registration.Mode == mode && registration.Apply != nil {
			return registration.Apply, true
		}
	}
	return nil, false
}

func lookupProductMetricsResolver(key productMetricsResolverKey) (productMetricsDeferredResolver, bool) {
	for _, registration := range productMetricsResolverRegistry {
		if registration.Key == key && registration.Resolve != nil {
			return registration.Resolve, true
		}
	}
	return nil, false
}

func classificationFromSynthetic(reason productMetricsExclusionReason) productMetricsClassification {
	for _, entry := range generatedProductMetricsSyntheticCensus {
		if reason != "" && entry.Exclusion == reason {
			return classificationFromCensusEntry(entry)
		}
		if reason == "" && entry.ID == productMetricsCommandUnknown {
			return classificationFromCensusEntry(entry)
		}
	}
	return failClosedProductMetricsClassification()
}

func classificationFromSyntheticPack() productMetricsClassification {
	for _, entry := range generatedProductMetricsSyntheticCensus {
		if entry.ID == productMetricsCommandPackCommand {
			return classificationFromCensusEntry(entry)
		}
	}
	return failClosedProductMetricsClassification()
}

func classificationFromCensusEntry(entry productMetricsCommandCensusEntry) productMetricsClassification {
	return productMetricsClassification{ID: entry.ID, Notice: entry.Notice, Recording: entry.Recording, Owner: entry.Owner, Exclusion: entry.Exclusion, Resolver: entry.Resolver}
}

func failClosedProductMetricsClassification() productMetricsClassification {
	return productMetricsClassification{Notice: productMetricsNoticeIneligible, Recording: productMetricsRecordingExcluded, Owner: productMetricsOwnerExcluded, Exclusion: productMetricsExclusionCensusMismatch}
}

func applyGenericMachineOutputPolicy(command *cobra.Command, args productMetricsInvocationArgs, _ productMetricsPolicyContext) (productMetricsPolicyDecision, bool) {
	_, jsonRequested := filterJSONFlag(args.Raw)
	_, schemaRequested := parseJSONSchemaRequest(args.Raw)
	matched := schemaRequested || jsonRequested
	if !commandHasProductMetricsConditional(command, productMetricsConditionalBeadsMachineOutput) {
		matched = matched || literalBoolFlagEnabled(command, args.Command, "json") || literalStringFlagIn(command, args.Command, "format", "json", "jsonl", "toon")
	}
	return ineligibleRecordablePolicy(), matched
}

func commandHasProductMetricsConditional(command *cobra.Command, want productMetricsConditionalMode) bool {
	if command == nil {
		return false
	}
	for _, encoded := range strings.Split(command.Annotations[productMetricsConditionalAnnotation], ",") {
		if productMetricsConditionalMode(encoded) == want {
			return true
		}
	}
	return false
}

func applyBeadsMachineOutputPolicy(_ *cobra.Command, args productMetricsInvocationArgs, _ productMetricsPolicyContext) (productMetricsPolicyDecision, bool) {
	format, _ := parseBeadFormat(args.Command)
	matched := format == "json" || format == "jsonl" || format == "toon"
	return ineligibleRecordablePolicy(), matched
}

func literalBoolFlagEnabled(command *cobra.Command, args []string, name string) bool {
	matched := false
	visitLiteralFlags(command, args, func(flagName, value string, hasValue bool) {
		if flagName != name {
			return
		}
		if !hasValue {
			matched = true
			return
		}
		parsed, err := strconv.ParseBool(value)
		matched = err == nil && parsed
	})
	return matched
}

func literalStringFlagNonempty(command *cobra.Command, args []string, name string) bool {
	value, present := literalStringFlagValue(command, args, name)
	return present && value != ""
}

func literalStringFlagIn(command *cobra.Command, args []string, name string, values ...string) bool {
	value, present := literalStringFlagValue(command, args, name)
	if !present {
		return false
	}
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func literalStringFlagValue(command *cobra.Command, args []string, name string) (string, bool) {
	lastValue := ""
	present := false
	visitLiteralFlags(command, args, func(flagName, flagValue string, hasValue bool) {
		if flagName == name {
			present = hasValue
			if hasValue {
				lastValue = flagValue
			}
		}
	})
	return lastValue, present
}

func isKnownProductMetricsCommandID(id productMetricsCommandID) bool {
	switch id {
	case productMetricsCommandHelp, productMetricsCommandVersion, productMetricsCommandUnknown, productMetricsCommandPackCommand:
		return true
	}
	for _, entry := range generatedProductMetricsCommandCensus {
		if entry.ID == id {
			return true
		}
	}
	return false
}

func productMetricsBuiltInPath(path string) bool {
	for _, entry := range generatedProductMetricsCommandCensus {
		if entry.Path == path {
			return true
		}
	}
	return false
}

func visitLiteralFlags(command *cobra.Command, args []string, visit func(string, string, bool)) {
	if command == nil || command.DisableFlagParsing {
		return
	}
	terminated := false
	for index := 0; index < len(args); index++ {
		token := args[index]
		if !terminated && token == "--" {
			terminated = true
			continue
		}
		if terminated || !strings.HasPrefix(token, "-") || token == "-" {
			continue
		}
		parsedFlags, consumed, ok := parseProductMetricsLiteralFlagToken(command, token, args[index+1:])
		if !ok {
			return
		}
		index += consumed
		for _, parsed := range parsedFlags {
			visit(parsed.name, parsed.value, true)
		}
	}
}

func splitLongFlag(token string) (name, value string, hasValue bool) {
	if !strings.HasPrefix(token, "--") || token == "--" {
		return "", "", false
	}
	name = strings.TrimPrefix(token, "--")
	if before, after, found := strings.Cut(name, "="); found {
		return before, after, true
	}
	return name, "", false
}

func lookupCommandFlag(command *cobra.Command, name string) *pflag.Flag {
	for current := command; current != nil; current = current.Parent() {
		if flag := current.Flags().Lookup(name); flag != nil {
			return flag
		}
		if flag := current.PersistentFlags().Lookup(name); flag != nil {
			return flag
		}
	}
	return nil
}

func lookupCommandShorthand(command *cobra.Command, shorthand string) *pflag.Flag {
	for current := command; current != nil; current = current.Parent() {
		if flag := current.Flags().ShorthandLookup(shorthand); flag != nil {
			return flag
		}
		if flag := current.PersistentFlags().ShorthandLookup(shorthand); flag != nil {
			return flag
		}
	}
	return nil
}
