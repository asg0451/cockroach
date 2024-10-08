// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

import d3 from "d3";
import React from "react";

type Chart<T> = (sel: d3.Selection<T>) => void;

/**
 * createChartComponent wraps a D3 reusable chart in a React component.
 * See https://bost.ocks.org/mike/chart/
 */
export default function createChartComponent<T>(
  containerTy: string,
  chart: Chart<T>,
) {
  return class WrappedChart extends React.Component<T> {
    containerEl: React.RefObject<Element> = React.createRef();

    componentDidMount() {
      this.redraw();
      this.addResizeHandler();
    }

    componentWillUnmount() {
      this.removeResizeHandler();
    }

    shouldComponentUpdate(props: T) {
      this.redraw(props);

      return false;
    }

    redraw(props: T = this.props) {
      d3.select(this.containerEl.current).datum(props).call(chart);
    }

    handleResize = () => {
      this.redraw();
    };

    addResizeHandler() {
      window.addEventListener("resize", this.handleResize);
    }

    removeResizeHandler() {
      window.removeEventListener("resize", this.handleResize);
    }

    render() {
      return React.createElement(containerTy, { ref: this.containerEl });
    }
  };
}
