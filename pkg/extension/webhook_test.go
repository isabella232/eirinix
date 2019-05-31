package extension_test

import (
	"context"

	. "github.com/SUSE/eirinix/pkg/extension"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission/types"
)

var _ = Describe("WebHook implementation", func() {
	e := &TestExtension{ParentExtension{Name: "test"}}
	w := NewWebHook(e)

	Context("With a fake extension", func() {
		It("It errors without a manager", func() {
			_, err := w.RegisterAdmissionWebHook(WebHookOptions{Id: "volume", Namespace: "eirini"})
			Expect(err).To(Not(BeNil()))
		})

		It("Delegates to the Extension the handler", func() {
			ctx := context.Background()
			t := types.Request{}
			res := w.Handle(ctx, t)
			annotations := res.Response.AuditAnnotations
			v, ok := annotations["name"]
			Expect(ok).To(Equal(true))
			Expect(v).To(Equal("test"))
		})
	})
})