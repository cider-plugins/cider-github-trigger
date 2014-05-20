# cider-github-trigger #

Build GitHub pull requests with Cider

This Cider plugin listens for `github.pull_request` events and triggers Cider
builds of the relevant repositories. It saves the output into Redis. The output
is then publicly accessible using a built-in web server. Once the build is
finished, `cider.build.finished` event is emitted, which contains the build
results as well as the URL that can be used to access the output.

## Dependencies ##

The following agents/plugins need to be installed for this plugin to work:

* [meeko-collector-github](https://github.com/meeko-contrib/meeko-collector-github)

## Status ##

This agent is rather usable, but the output event kind can change.

The code is quite horrible and needs to be heavily refactored.

## License ##

MIT, see the `LICENSE` file.
