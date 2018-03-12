package http

import (
	"fmt"

	extv1beta1 "k8s.io/api/extensions/v1beta1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/ingress/core/pkg/ingress/annotations/class"

	"github.com/golang/glog"
	"github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha1"
	"github.com/jetstack/cert-manager/pkg/issuer/acme/http/solver"
)

// getIngressesForCertificate returns a list of Ingresses that were created to solve
// http challenges for the given domain
func (s *Solver) getIngressesForCertificate(crt *v1alpha1.Certificate, domain string) ([]*extv1beta1.Ingress, error) {
	if crt.Status.ACME.OrderURL == "" {
		return []*extv1beta1.Ingress{}, nil
	}
	podLabels := podLabels(crt, domain)
	selector := labels.NewSelector()
	for key, val := range podLabels {
		req, err := labels.NewRequirement(key, selection.Equals, []string{val})
		if err != nil {
			return nil, err
		}
		selector = selector.Add(*req)
	}

	ingressList, err := s.ingressLister.Ingresses(crt.Namespace).List(selector)
	if err != nil {
		return nil, err
	}

	var relevantIngresses []*extv1beta1.Ingress
	for _, ingress := range ingressList {
		if !metav1.IsControlledBy(ingress, crt) {
			glog.Infof("Found ingress %q with acme-order-url annotation set to that of Certificate %q"+
				"but it is not owned by the Certificate resource, so skipping it.", ingress.Name, crt.Name)
			continue
		}
		if ingress.Labels == nil ||
			ingress.Labels[domainLabelKey] != domain {
			continue
		}
		relevantIngresses = append(relevantIngresses, ingress)
	}

	return relevantIngresses, nil
}

// ensureIngress will ensure the ingress required to solve this challenge
// exists, or if an existing ingress is specified on the secret will ensure
// that the ingress has an appropriate challenge path configured
func (s *Solver) ensureIngress(crt *v1alpha1.Certificate, svcName, domain, token string) (ing *extv1beta1.Ingress, err error) {
	domainCfg := crt.Spec.ACME.ConfigForDomain(domain)
	if domainCfg == nil {
		return nil, fmt.Errorf("no ACME challenge configuration found for domain %q", domain)
	}
	httpDomainCfg := domainCfg.HTTP01
	if httpDomainCfg == nil {
		httpDomainCfg = &v1alpha1.ACMECertificateHTTP01Config{}
	}
	if httpDomainCfg != nil &&
		httpDomainCfg.Ingress != "" {
		return s.addChallengePathToIngress(crt, svcName, domain, token, *httpDomainCfg)
	}
	return s.createIngress(crt, svcName, domain, token, *httpDomainCfg)
}

// createIngress will create a challenge solving pod for the given certificate,
// domain, token and key.
func (s *Solver) createIngress(crt *v1alpha1.Certificate, svcName, domain, token string, domainCfg v1alpha1.ACMECertificateHTTP01Config) (*extv1beta1.Ingress, error) {
	podLabels := podLabels(crt, domain)
	// TODO: add additional annotations to help workaround problematic ingress controller behaviours
	ingAnnotaions := make(map[string]string)
	if ingClass := domainCfg.IngressClass; ingClass != nil {
		ingAnnotaions[class.IngressKey] = *ingClass
	}

	ingPathToAdd := ingressPath(token, svcName)

	return s.client.ExtensionsV1beta1().Ingresses(crt.Namespace).Create(&extv1beta1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "cm-acme-http-solver-",
			Namespace:    crt.Namespace,
			Labels:       podLabels,
			Annotations:  ingAnnotaions,
		},
		Spec: extv1beta1.IngressSpec{
			Rules: []extv1beta1.IngressRule{
				{
					Host: domain,
					IngressRuleValue: extv1beta1.IngressRuleValue{
						HTTP: &extv1beta1.HTTPIngressRuleValue{
							Paths: []extv1beta1.HTTPIngressPath{ingPathToAdd},
						},
					},
				},
			},
		},
	})
}

func (s *Solver) addChallengePathToIngress(crt *v1alpha1.Certificate, svcName, domain, token string, domainCfg v1alpha1.ACMECertificateHTTP01Config) (*extv1beta1.Ingress, error) {
	ing, err := s.ingressLister.Ingresses(crt.Namespace).Get(domainCfg.Ingress)
	if err != nil {
		return nil, err
	}

	ingPathToAdd := ingressPath(token, svcName)
	// check for an existing Rule for the given domain on the ingress resource
	for _, rule := range ing.Spec.Rules {
		if rule.Host == domain {
			if rule.HTTP == nil {
				rule.HTTP = &extv1beta1.HTTPIngressRuleValue{}
			}
			for i, p := range rule.HTTP.Paths {
				// if an existing path exists on this rule for the challenge path,
				// we overwrite it else we'll confuse ingress controllers
				if p.Path == ingPathToAdd.Path {
					rule.HTTP.Paths[i] = ingPathToAdd
					return s.client.ExtensionsV1beta1().Ingresses(ing.Namespace).Update(ing)
				}
			}
			rule.HTTP.Paths = append(rule.HTTP.Paths, ingPathToAdd)
			return s.client.ExtensionsV1beta1().Ingresses(ing.Namespace).Update(ing)
		}
	}

	// if one doesn't exist, create a new IngressRule
	ing.Spec.Rules = append(ing.Spec.Rules, extv1beta1.IngressRule{
		Host: domain,
		IngressRuleValue: extv1beta1.IngressRuleValue{
			HTTP: &extv1beta1.HTTPIngressRuleValue{
				Paths: []extv1beta1.HTTPIngressPath{ingPathToAdd},
			},
		},
	})
	return s.client.ExtensionsV1beta1().Ingresses(ing.Namespace).Update(ing)
}

// cleanupIngresses will remove the rules added by cert-manager to an existing
// ingress, or delete the ingress if an existing ingress name is not specified
// on the certificate.
func (s *Solver) cleanupIngresses(crt *v1alpha1.Certificate, domain, token string) error {
	domainCfg := crt.Spec.ACME.ConfigForDomain(domain)
	httpDomainCfg := domainCfg.HTTP01
	if httpDomainCfg == nil {
		httpDomainCfg = &v1alpha1.ACMECertificateHTTP01Config{}
	}
	existingIngressName := httpDomainCfg.Ingress

	// if the 'ingress' field on the domain config is not set, we need to delete
	// the ingress resources that cert-manager has created to solve the challenge
	if existingIngressName == "" {
		ingresses, err := s.getIngressesForCertificate(crt, domain)
		if err != nil {
			return err
		}
		var errs []error
		for _, ingress := range ingresses {
			// TODO: should we call DeleteCollection here? We'd need to somehow
			// also ensure ownership as part of that request using a FieldSelector.
			err := s.client.ExtensionsV1beta1().Ingresses(ingress.Namespace).Delete(ingress.Name, nil)
			if err != nil {
				errs = append(errs, err)
			}
		}
		return utilerrors.NewAggregate(errs)
	}

	// otherwise, we need to remove any cert-manager added rules from the ingress resource
	ing, err := s.client.ExtensionsV1beta1().Ingresses(crt.Namespace).Get(existingIngressName, metav1.GetOptions{})
	if k8sErrors.IsNotFound(err) {
		glog.Infof("attempt to cleanup Ingress %q of ACME challenge path failed: %v", existingIngressName, err)
		return nil
	}
	if err != nil {
		return err
	}

	ingPathToDel := solverPathFn(token)
Outer:
	for _, rule := range ing.Spec.Rules {
		if rule.Host == domain {
			if rule.HTTP == nil {
				return nil
			}
			for i, path := range rule.HTTP.Paths {
				if path.Path == ingPathToDel {
					rule.HTTP.Paths = append(rule.HTTP.Paths[:i], rule.HTTP.Paths[i+1:]...)
					break Outer
				}
			}
		}
	}

	_, err = s.client.ExtensionsV1beta1().Ingresses(ing.Namespace).Update(ing)
	if err != nil {
		return err
	}

	return nil
}

// ingressPath returns the ingress HTTPIngressPath object needed to solve this
// challenge.
func ingressPath(token, serviceName string) extv1beta1.HTTPIngressPath {
	return extv1beta1.HTTPIngressPath{
		Path: solverPathFn(token),
		Backend: extv1beta1.IngressBackend{
			ServiceName: serviceName,
			ServicePort: intstr.FromInt(acmeSolverListenPort),
		},
	}
}

var solverPathFn = func(token string) string {
	return fmt.Sprintf("%s/%s", solver.HTTPChallengePath, token)
}
