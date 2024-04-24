## gatewayd completion zsh

Generate the autocompletion script for zsh

### Synopsis

Generate the autocompletion script for the zsh shell.

If shell completion is not already enabled in your environment you will need
to enable it.  You can execute the following once:

	echo "autoload -U compinit; compinit" >> ~/.zshrc

To load completions in your current shell session:

	source <(gatewayd completion zsh)

To load completions for every new session, execute once:

#### Linux:

	gatewayd completion zsh > "${fpath[1]}/_gatewayd"

#### macOS:

	gatewayd completion zsh > $(brew --prefix)/share/zsh/site-functions/_gatewayd

You will need to start a new shell for this setup to take effect.


```
gatewayd completion zsh [flags]
```

### Options

```
  -h, --help              help for zsh
      --no-descriptions   disable completion descriptions
```

### SEE ALSO

* [gatewayd completion](gatewayd_completion.md)	 - Generate the autocompletion script for the specified shell

###### Auto generated by spf13/cobra on 8-Mar-2024