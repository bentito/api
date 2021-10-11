package internal

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	semver "github.com/blang/semver/v4"
	"github.com/operator-framework/api/pkg/manifests"
	"github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/operator-framework/api/pkg/validation/errors"
	interfaces "github.com/operator-framework/api/pkg/validation/interfaces"
)

// OperatorHubValidator validates the bundle manifests against the required criteria to publish
// the projects on OperatorHub.io.
//
// Note that this validator allows to receive a List of optional values as key=values. Currently, only the
// `k8s-version` key is allowed. If informed, it will perform the checks against this specific Kubernetes version where the
// operator bundle is intend to be distribute for.
var OperatorHubValidator interfaces.Validator = interfaces.ValidatorFunc(validateOperatorHub)

var validCapabilities = map[string]struct{}{
	"Basic Install":     {},
	"Seamless Upgrades": {},
	"Full Lifecycle":    {},
	"Deep Insights":     {},
	"Auto Pilot":        {},
}

var validMediatypes = map[string]struct{}{
	"image/gif":     {},
	"image/jpeg":    {},
	"image/png":     {},
	"image/svg+xml": {},
}

var validCategories = map[string]struct{}{
	"AI/Machine Learning":    {},
	"Application Runtime":    {},
	"Big Data":               {},
	"Cloud Provider":         {},
	"Developer Tools":        {},
	"Database":               {},
	"Integration & Delivery": {},
	"Logging & Tracing":      {},
	"Monitoring":             {},
	"Networking":             {},
	"OpenShift Optional":     {},
	"Security":               {},
	"Storage":                {},
	"Streaming & Messaging":  {},
}

func validateOperatorHub(objs ...interface{}) (results []errors.ManifestResult) {

	// Obtain the k8s version if informed via the objects an optional
	k8sVersion := ""
	for _, obj := range objs {
		switch obj.(type) {
		case map[string]string:
			k8sVersion = obj.(map[string]string)[k8sVersionKey]
			if len(k8sVersion) > 0 {
				break
			}
		}
	}

	for _, obj := range objs {
		switch v := obj.(type) {
		case *manifests.Bundle:
			results = append(results, validateBundleOperatorHub(v, k8sVersion))
		}
	}

	return results
}

func validateBundleOperatorHub(bundle *manifests.Bundle, k8sVersion string) errors.ManifestResult {
	result := errors.ManifestResult{Name: bundle.Name}

	if bundle == nil {
		result.Add(errors.ErrInvalidBundle("Bundle is nil", nil))
		return result
	}

	if bundle.CSV == nil {
		result.Add(errors.ErrInvalidBundle("Bundle csv is nil", bundle.Name))
		return result
	}

	sizeTooLargeResult := checkBundleSize(bundle)
	for _, err := range sizeTooLargeResult.errs {
		result.Add(errors.ErrInvalidBundle(err.Error(), bundle.Name))
	}
	for _, warn := range sizeTooLargeResult.warns {
		result.Add(errors.ErrInvalidBundle(warn.Error(), bundle.Name))
	}

	csvChecksResult := validateHubCSVSpec(*bundle.CSV)
	for _, err := range csvChecksResult.errs {
		result.Add(errors.ErrInvalidCSV(err.Error(), bundle.CSV.GetName()))
	}
	for _, warn := range csvChecksResult.warns {
		result.Add(errors.WarnInvalidCSV(warn.Error(), bundle.CSV.GetName()))
	}

	errs, warns := validateDeprecatedAPIS(bundle, k8sVersion)
	for _, err := range errs {
		result.Add(errors.ErrFailedValidation(err.Error(), bundle.CSV.GetName()))
	}
	for _, warn := range warns {
		result.Add(errors.WarnFailedValidation(warn.Error(), bundle.CSV.GetName()))
	}

	if warn := validateHubChannels(bundle.Channels); warn != nil {
		result.Add(errors.WarnFailedValidation(warn.Error(), bundle.CSV.GetName()))
	}

	return result
}

// validateHubChannels will check the channels. The motivation for the following check is to ensure that operators
// authors knows if their operator bundles are or not respecting the Naming Convention Rules.
// However, the operator authors still able to choose the names as please them.
func validateHubChannels(channels []string) error {
	const candidate = "candidate"
	const stable = "stable"
	const fast = "fast"

	var channelsNotFollowingConventional []string
	for _, channel := range channels {
		if !strings.HasPrefix(channel, candidate) &&
			!strings.HasPrefix(channel, stable) &&
			!strings.HasPrefix(channel, fast) {
			channelsNotFollowingConventional = append(channelsNotFollowingConventional, channel)
		}

	}

	if len(channelsNotFollowingConventional) > 0 {
		return fmt.Errorf("channel(s) %+q are not following the recommended naming convention: "+
			"https://olm.operatorframework.io/docs/best-practices/channel-naming",
			channelsNotFollowingConventional)
	}

	return nil
}

// validateHubCSVSpec will check the CSV against the criteria to publish an
// operator bundle in the OperatorHub.io
func validateHubCSVSpec(csv v1alpha1.ClusterServiceVersion) CSVChecks {
	checks := CSVChecks{csv: csv, errs: []error{}, warns: []error{}}

	checks = checkSpecProviderName(checks)
	checks = checkSpecMaintainers(checks)
	checks = checkSpecLinks(checks)
	checks = checkAnnotations(checks)
	checks = checkSpecVersion(checks)
	checks = checkSpecIcon(checks)
	checks = checkSpecMinKubeVersion(checks)
	checks = checkBundleName(checks)

	return checks
}

type CSVChecks struct {
	csv   v1alpha1.ClusterServiceVersion
	errs  []error
	warns []error
}

// checkBundleName will validate the operator bundle name informed via CSV.metadata.name.
// The motivation for the following check is to ensure that operators authors knows that operator bundles names should
// follow a name and versioning convention
func checkBundleName(checks CSVChecks) CSVChecks {

	// Check if is following the semver
	re := regexp.MustCompile("([0-9]+)\\.([0-9]+)\\.([0-9]+)(?:-([0-9A-Za-z-]+(?:\\.[0-9A-Za-z-]+)*))?(?:\\+[0-9A-Za-z-]+)?$")
	match := re.FindStringSubmatch(checks.csv.Name)

	if len(match) > 0 {
		if _, err := semver.Parse(match[0]); err != nil {
			checks.warns = append(checks.warns, fmt.Errorf("csv.metadata.Name %v is not following the versioning "+
				"convention (MAJOR.MINOR.PATCH e.g 0.0.1): https://semver.org/", checks.csv.Name))
		}
	} else {
		checks.warns = append(checks.warns, fmt.Errorf("csv.metadata.Name %v is not following the versioning "+
			"convention (MAJOR.MINOR.PATCH e.g 0.0.1): https://semver.org/", checks.csv.Name))
	}

	// Check if its following the name convention
	if len(strings.Split(checks.csv.Name, ".v")) < 2 {
		checks.warns = append(checks.errs, fmt.Errorf("csv.metadata.Name %v is not following the recommended "+
			"naming convention: <operator-name>.v<semver> e.g. memcached-operator.v0.0.1", checks.csv.Name))
	}

	return checks
}

// checkSpecMinKubeVersion will validate the spec minKubeVersion informed via CSV.spec.minKubeVersion
func checkSpecMinKubeVersion(checks CSVChecks) CSVChecks {
	if len(strings.TrimSpace(checks.csv.Spec.MinKubeVersion)) == 0 {
		checks.warns = append(checks.warns, fmt.Errorf(minKubeVersionWarnMessage))
	} else {
		if _, err := semver.ParseTolerant(checks.csv.Spec.MinKubeVersion); err != nil {
			checks.errs = append(checks.errs, fmt.Errorf("csv.Spec.MinKubeVersion has an invalid value: %s", checks.csv.Spec.MinKubeVersion))
		}
	}
	return checks
}

// checkSpecVersion will validate the spec Version informed via CSV.spec.Version
func checkSpecVersion(checks CSVChecks) CSVChecks {
	// spec.Version needs to be set
	if checks.csv.Spec.Version.Equals(semver.Version{}) {
		checks.errs = append(checks.errs, fmt.Errorf("csv.Spec.Version must be set"))
	}
	return checks
}

// checkAnnotations will validate the values informed via annotations such as; capabilities and categories
func checkAnnotations(checks CSVChecks) CSVChecks {
	if checks.csv.GetAnnotations() == nil {
		checks.csv.SetAnnotations(make(map[string]string))
	}

	if capability, ok := checks.csv.ObjectMeta.Annotations["capabilities"]; ok {
		if _, ok := validCapabilities[capability]; !ok {
			checks.errs = append(checks.errs, fmt.Errorf("csv.Metadata.Annotations.Capabilities %s is not a valid capabilities level", capability))
		}
	}

	if categories, ok := checks.csv.ObjectMeta.Annotations["categories"]; ok {
		categorySlice := strings.Split(categories, ",")

		// use custom categories for validation if provided
		customCategoriesPath := os.Getenv("OPERATOR_BUNDLE_CATEGORIES")
		if customCategoriesPath != "" {
			customCategories, err := extractCategories(customCategoriesPath)
			if err != nil {
				checks.errs = append(checks.errs, fmt.Errorf("could not extract custom categories from categories %#v: %s", customCategories, err))
			} else {
				for _, category := range categorySlice {
					if _, ok := customCategories[strings.TrimSpace(category)]; !ok {
						checks.errs = append(checks.errs, fmt.Errorf("csv.Metadata.Annotations[\"categories\"] value %s is not in the set of custom categories", category))
					}
				}
			}
		} else {
			// use default categories
			for _, category := range categorySlice {
				if _, ok := validCategories[strings.TrimSpace(category)]; !ok {
					checks.errs = append(checks.errs, fmt.Errorf("csv.Metadata.Annotations[\"categories\"] value %s is not in the set of default categories", category))
				}
			}
		}
	}
	return checks
}

// checkSpecIcon will validate if the CSV.spec.Icon was informed and is correct
func checkSpecIcon(checks CSVChecks) CSVChecks {
	if checks.csv.Spec.Icon != nil {
		// only one icon is allowed
		if len(checks.csv.Spec.Icon) != 1 {
			checks.errs = append(checks.errs, fmt.Errorf("csv.Spec.Icon should only have one element"))
		}

		icon := checks.csv.Spec.Icon[0]
		if icon.MediaType == "" || icon.Data == "" {
			checks.errs = append(checks.errs, fmt.Errorf("csv.Spec.Icon elements should contain both data and mediatype"))
		}

		if icon.MediaType != "" {
			if _, ok := validMediatypes[icon.MediaType]; !ok {
				checks.errs = append(checks.errs, fmt.Errorf("csv.Spec.Icon %s does not have a valid mediatype", icon.MediaType))
			}
		}
	} else {
		checks.warns = append(checks.warns, fmt.Errorf("csv.Spec.Icon not specified"))
	}
	return checks
}

// checkSpecLinks will validate the value informed via csv.Spec.Links
func checkSpecLinks(checks CSVChecks) CSVChecks {
	for _, link := range checks.csv.Spec.Links {
		if link.Name == "" || link.URL == "" {
			checks.errs = append(checks.errs, fmt.Errorf("csv.Spec.Links elements should contain both name and url"))
		}
		if link.URL != "" {
			_, err := url.ParseRequestURI(link.URL)
			if err != nil {
				checks.errs = append(checks.errs, fmt.Errorf("csv.Spec.Links url %s is invalid: %v", link.URL, err))
			}
		}
	}
	return checks
}

// checkSpecMaintainers will validate the values informed via csv.Spec.Maintainers
func checkSpecMaintainers(checks CSVChecks) CSVChecks {
	for _, maintainer := range checks.csv.Spec.Maintainers {
		if maintainer.Name == "" || maintainer.Email == "" {
			checks.errs = append(checks.errs, fmt.Errorf("csv.Spec.Maintainers elements should contain both name and email"))
		}
		if maintainer.Email != "" {
			_, err := mail.ParseAddress(maintainer.Email)
			if err != nil {
				checks.errs = append(checks.errs, fmt.Errorf("csv.Spec.Maintainers email %s is invalid: %v", maintainer.Email, err))
			}
		}
	}
	return checks
}

// checkSpecProviderName will validate the values informed via csv.Spec.Provider.Name
func checkSpecProviderName(checks CSVChecks) CSVChecks {
	if checks.csv.Spec.Provider.Name == "" {
		checks.errs = append(checks.errs, fmt.Errorf("csv.Spec.Provider.Name not specified"))
	}
	return checks
}

type categories struct {
	Contents []string `json:"categories"`
}

// extractCategories reads a custom categories file and returns the contents in a map[string]struct{}
func extractCategories(path string) (map[string]struct{}, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("finding category file: %w", err)
	}

	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading category file: %w", err)
	}

	cat := categories{}
	err = json.Unmarshal(data, &cat)
	if err != nil {
		return nil, fmt.Errorf("unmarshaling category file: %w", err)
	}

	customCategories := make(map[string]struct{})
	for _, c := range cat.Contents {
		customCategories[c] = struct{}{}
	}
	return customCategories, nil
}

func checkBundleSize(bundle *manifests.Bundle) BundleChecks {
	// Crawl the bundle, mounted at ./bundle, and add up the bytes
	// Should we do something with the bundle parameter?
	checks := BundleChecks{errs: []error{}, warns: []error{}}
	bundleDir := "./bundle"
	abs, absErr := filepath.Abs(bundleDir)
	if absErr != nil {
		checks.errs = append(checks.errs, fmt.Errorf("could not get absolute path to bundle dir: %w", absErr))
	}
	errChDir := os.Chdir(abs)
	if errChDir != nil {
		checks.errs = append(checks.errs, fmt.Errorf("could not change to bundle dir: %w", errChDir))
	}
	size, dirSizeErr := dirSize(abs)
	if dirSizeErr != nil {
		checks.errs = append(checks.errs, fmt.Errorf("could not check size of bundle: %w", dirSizeErr))
	}
	checkSize(size, checks)
	return checks
}

type BundleChecks struct {
	errs  []error
	warns []error
}

func checkSize(size int64, checks BundleChecks) (bundleChecks BundleChecks) {
	warnPercent := 0.15
	if size > 1048576 {
		checks.errs = append(checks.errs,
			fmt.Errorf("total bundle size of %s is greater than 1,048,576 bytes will not work yet",
				strconv.FormatInt(size, 10)))
	}
	warnSize := int64(1048576 - math.Round(1048576*warnPercent))
	if size > warnSize {
		checks.errs = append(checks.errs,
			fmt.Errorf("total bundle size of %s is greater than %d bytes, close to the current 1,048,576 byte limit",
				strconv.FormatInt(size, 10), warnSize))
	}
	return checks
}

func dirSize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return err
	})
	return size, err
}
