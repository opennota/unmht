unmht [![License](http://img.shields.io/:license-gpl3-blue.svg)](http://www.gnu.org/licenses/gpl-3.0.html) [![Build Status](https://travis-ci.org/opennota/unmht.png?branch=master)](https://travis-ci.org/opennota/unmht)
=====

Currently (08-May-2018), the
[UnMHT](https://addons.mozilla.org/en-US/firefox/addon/unmht/) add-on doesn't
work with Firefox Quantum; nor there is another add-on which supports opening
existing MHT files.  So here is a command-line tool which allows one to view
previously saved MHT web archives in a browser.

## Install

    go get -u github.com/opennota/unmht

## Use

    unmht previously-saved.mht
