### Fixed
- Windows browser shutdown retains bounded, exact-owned process-generation handles and checks their exit signals before reporting completion. Profile/job membership alone no longer establishes completion; native failures stay explicit, with no additional teardown grace.
