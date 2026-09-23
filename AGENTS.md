# AGENTS.md

Read [README.md](README.md).

# Environment
* Go 1.27

# General rules
* Do not preserve backward compatibility.
* Choose the simplest implementation that fully meets the current requirements.
* Prefer established, well-maintained libraries over custom implementations.
* Fix the cause, not the symptom.
* Suggest best practices, even if they may require refactoring.

# Development workflow
* Always write a comprehensive test suite covering the implementation alongside the implementation itself.
* Run `make fix gofix check` after completing the implementation.

# Docs
* Follow the principles of Maxim Ilyakhov's "Write, Cut": be concise without losing substance.
* Prefer clear structure, diagrams, and lists over long blocks of text.
